// Tests for step execution: the first failure stops it, a forking step does not block it, a trigger runs only
// its own steps, a task list shares its trigger, and a command step reaches its command.
//
//	step, shell, loadDoc, names, loadSteps
//	TestForkingStepDoesNotBlock  A step that forks a child and exits must not hold the workflow open.
//	TestStopsAtFirstFailure
//	TestTriggers                 A signal task runs its steps on the signal; Start runs start then ready at once
//	                             with no tunnel; an unknown trigger is refused at load.
//	TestTaskList                 Every task under one on runs on that trigger, in order.
//	TestCommands                 An action no enabled subsystem offers is refused at load; a command sees the step's
//	                             params and fails the step outside 2xx.
package workflow

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/router"
)

type step struct{ on, action, command string }

func shell(command string) step { return step{"", "shell.exec", command} }

func loadDoc(t *testing.T, doc string, commands map[string]router.Command) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, commands)
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
	doc := "name: t\nsteps:\n"
	for i, s := range steps {
		doc += fmt.Sprintf("  - action: %s\n    name: s%d\n    on: %q\n    params:\n      command: %q\n", s.action, i+1, s.on, s.command)
	}
	return loadDoc(t, doc, Commands)
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
	if sigs := Signals(); !slices.Equal(sigs, []string{"SIGUSR1"}) {
		t.Fatalf("Signals = %v, want SIGUSR1 alone", sigs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := WatchSignal("SIGUSR1")
	done := make(chan error, 1)
	go func() { done <- task(ctx) }()
	_ = syscall.Kill(os.Getpid(), syscall.SIGUSR1)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "usr1")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the SIGUSR1 step never ran")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"ready", "start", "usr1"}) {
		t.Fatalf("ran %v; want the signal step alone, then start and ready with no tunnel", got)
	}
	if err := loadSteps(t, mark("SIGKILL", "x")); err == nil || !strings.Contains(err.Error(), "unknown trigger") {
		t.Fatalf("an unknown trigger must be refused at load, got %v", err)
	}
}

func TestTaskList(t *testing.T) {
	dir := t.TempDir()
	doc := fmt.Sprintf(`name: t
steps:
  - on: stop
    tasks:
      - action: shell.exec
        params: {command: /usr/bin/touch %[1]s/a}
      - action: shell.exec
        params: {command: /usr/bin/touch %[1]s/b}
  - action: shell.exec
    params: {command: /usr/bin/touch %[1]s/c}
`, dir)
	if err := loadDoc(t, doc, Commands); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[0].On != "stop" || steps[1].On != "stop" || steps[2].On != "start" {
		t.Fatalf("loaded %+v", steps)
	}
	if err := Run(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ran %v; want the two stop tasks alone", got)
	}
}

func TestCommands(t *testing.T) {
	var got []string
	command := func(status int) router.Command {
		return func(_ context.Context, params map[string]any) (int, any, string) {
			got = append(got, fmt.Sprint(params))
			return status, map[string]string{"id": "s-1"}, ""
		}
	}
	commands := map[string]router.Command{"vscode.sessions.start": command(http.StatusCreated), "jupyter.sessions.stop": command(http.StatusNotFound)}
	doc := `name: t
steps:
  - name: create
    action: vscode.sessions.start
    params: {authorized_key: k}
  - name: missing
    action: jupyter.sessions.stop
    params: {id: j-9}
`
	if err := loadDoc(t, doc, nil); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("a command no enabled subsystem offers must be refused at load, got %v", err)
	}
	if err := loadDoc(t, doc, commands); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a status outside 2xx must fail the step, got %v", err)
	}
	if !slices.Equal(got, []string{"map[authorized_key:k]", "map[id:j-9]"}) {
		t.Fatalf("calls = %q", got)
	}
}
