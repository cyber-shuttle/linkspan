// Tests for step execution: the first failure stops it, a forking step does not block it, a trigger runs only
// its own steps, a trigger's steps run in order, and a subsystem's action sees its step.
//
//	step, shell, loadDoc, names, loadSteps
//	TestForkingStepDoesNotBlock  A step that forks a child and exits must not hold the workflow open.
//	TestExecIsAProcess           A paused step ends the run; the false after it never runs.
//	TestJobEndsAfterReady        Start sends Linkspan SIGTERM once start and ready are run and the servers their
//	                             steps started have ended.
//	TestStopsAtFirstFailure
//	TestTriggers                 Start runs start then ready, then a signal's steps on the signal; an unknown trigger
//	                             is refused at load.
//	TestTriggerList              Every step under one trigger runs on it, in order; a document with no tasks,
//	                             or a field it does not know, is refused.
//	TestActions                  An action no subsystem offers is refused at load; an action sees the step's
//	                             params, its ref among them, and fails the step outside 2xx.
package workflow

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/servers"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

type step struct{ on, action, command string }

func shell(command string) step { return step{"", "shell.exec", command} }

func loadDoc(t *testing.T, doc string, actions map[string]router.Action) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, actions)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func loadSteps(t *testing.T, steps ...step) error {
	t.Helper()
	doc := "name: t\ntasks:\n"
	for i, s := range steps {
		doc += fmt.Sprintf("  - on: %q\n    steps:\n      - action: %s\n        name: s%d\n        params:\n          command: %q\n", s.on, s.action, i+1, s.command)
	}
	return loadDoc(t, doc, Actions)
}

func TestForkingStepDoesNotBlock(t *testing.T) {
	script := filepath.Join(t.TempDir(), "step.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 2 &\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := loadSteps(t, shell("sh "+script)); err != nil {
		t.Fatal(err)
	}

	ran := make(chan error, 1)
	go func() { ran <- Run(context.Background(), "start") }()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("step failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run blocked on a step that forked a long-lived child")
	}
}

func TestExecIsAProcess(t *testing.T) {
	status, body, msg := exec(context.Background(), map[string]any{"command": "true", "ref": "payload"})
	if status != http.StatusOK || body.(tasks.Task).ID != "payload" || body.(tasks.Task).Pid == 0 {
		t.Fatalf("exec answered %d %v %q, want 200 with the ended process", status, body, msg)
	}
	if err := loadSteps(t, shell("sleep 30"), shell("/usr/bin/false")); err != nil {
		t.Fatal(err)
	}
	loaded[0].Steps[0].Params["ref"] = "payload"
	ran := make(chan error, 1)
	go func() { ran <- Run(context.Background(), "start") }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if l := tasks.Select(servers.Process); len(l) == 1 && l[0].Pid != 0 {
			servers.Pausing("payload", true)
			_ = syscall.Kill(-l[0].Pid, syscall.SIGKILL)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the payload never ran")
		}
	}
	if err := <-ran; err != nil {
		t.Fatalf("a paused step must end its trigger without error, got %v", err)
	}
}

func TestJobEndsAfterReady(t *testing.T) {
	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGTERM)
	defer signal.Stop(got)
	serve := func(_ context.Context, params map[string]any) (int, any, string) {
		return http.StatusCreated, servers.Start(tasks.Task{Kind: "jupyter", ID: servers.Ref(params)}, "sleep", "30"), ""
	}
	doc := "name: t\ntasks:\n  - steps: [{action: shell.exec, params: {command: \"true\"}}]\n  - {on: ready, steps: [{ref: j, action: jupyter.sessions.start}]}\n"
	if err := loadDoc(t, doc, map[string]router.Action{"shell.exec": exec, "jupyter.sessions.start": serve}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Start()(ctx) }()
	select {
	case <-got:
		t.Fatal("the job must run while a server a step started runs")
	case <-time.After(500 * time.Millisecond):
	}
	if !tasks.Stop("j") {
		t.Fatal("the ready step's server must be listed")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the job must end once start and ready are done")
	}
}

func TestStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	first, third := filepath.Join(dir, "first"), filepath.Join(dir, "third")
	if err := loadSteps(t, shell("/usr/bin/touch "+first), shell("/usr/bin/false"), shell("/usr/bin/touch "+third)); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), "start"); err == nil {
		t.Fatal("a failing step must stop the workflow")
	}
	if _, err := os.Stat(first); err != nil {
		t.Error("the first step must have run")
	}
	if _, err := os.Stat(third); err == nil {
		t.Error("the step after the failure must not have run")
	}
}

func TestTriggers(t *testing.T) {
	dir := t.TempDir()
	mark := func(on, name string) step {
		return step{on, "shell.exec", "/usr/bin/touch " + filepath.Join(dir, name)}
	}
	if err := loadSteps(t, mark("", "start"), mark("ready", "ready"), mark("SIGUSR1", "usr1"), mark("stop", "stop")); err != nil {
		t.Fatal(err)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	ctx, cancel := context.WithCancel(context.Background())
	task := Start()
	done := make(chan error, 1)
	go func() { done <- task(ctx) }()
	_ = syscall.Kill(os.Getpid(), syscall.SIGUSR1)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if len(names(t, dir)) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ran %v; want start, ready and the SIGUSR1 step", names(t, dir))
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"ready", "start", "usr1"}) {
		t.Fatalf("ran %v; want start and ready, then the signal step", got)
	}
	if err := loadSteps(t, mark("SIGKILL", "x")); err == nil || !strings.Contains(err.Error(), "trigger 1: unknown") {
		t.Fatalf("an unknown trigger must be refused at load, got %v", err)
	}
}

func TestTriggerList(t *testing.T) {
	dir := t.TempDir()
	doc := fmt.Sprintf(`name: t
tasks:
  - on: stop
    steps:
      - action: shell.exec
        params: {command: /usr/bin/touch %[1]s/a}
      - action: shell.exec
        params: {command: /usr/bin/touch %[1]s/b}
`, dir)
	if err := loadDoc(t, doc+"bogus: 1\n", Actions); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("an unknown field must be refused, got %v", err)
	}
	if err := loadDoc(t, "name: t\ntasks: [{on: stop}]\n", Actions); err == nil || !strings.Contains(err.Error(), "no steps") {
		t.Fatalf("a trigger is one or more steps, got %v", err)
	}
	if err := loadDoc(t, "name: t\n", Actions); err == nil || !strings.Contains(err.Error(), "no tasks") {
		t.Fatalf("a workflow is one or more triggers, got %v", err)
	}
	if err := loadDoc(t, doc, Actions); err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].On != "stop" || len(loaded[0].Steps) != 2 {
		t.Fatalf("loaded %+v", loaded)
	}
	if err := Run(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ran %v; want the two stop steps alone", got)
	}
}

func TestActions(t *testing.T) {
	var got []string
	action := func(status int) router.Action {
		return func(_ context.Context, params map[string]any) (int, any, string) {
			got = append(got, fmt.Sprint(params))
			return status, map[string]string{"id": "s-1"}, ""
		}
	}
	actions := map[string]router.Action{"vscode.sessions.start": action(http.StatusCreated), "jupyter.sessions.stop": action(http.StatusNotFound)}
	doc := `name: t
tasks:
  - steps:
      - name: create
        ref: laptop
        action: vscode.sessions.start
        params: {authorized_key: k}
      - name: missing
        action: jupyter.sessions.stop
        params: {id: j-9}
`
	if err := loadDoc(t, doc, nil); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("an action no subsystem offers must be refused at load, got %v", err)
	}
	if err := loadDoc(t, doc, actions); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a status outside 2xx must fail the step, got %v", err)
	}
	if !slices.Equal(got, []string{"map[authorized_key:k ref:laptop]", "map[id:j-9]"}) {
		t.Fatalf("calls = %q", got)
	}
}
