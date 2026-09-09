// Tests for step execution: the first failure stops it, and a forking step
// does not block it.
//
//	step, shell, loadSteps       They build a document from steps and load it.
//	TestForkingStepDoesNotBlock  A step that forks a daemon, as cs-control's
//	                             setsid --fork does, must not hold the workflow
//	                             open.
//	TestStopsAtFirstFailure      The step after a failure must not run.
package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type step struct{ action, command string }

func shell(command string) step { return step{"shell.exec", command} }

func loadSteps(t *testing.T, steps ...step) *Workflow {
	t.Helper()
	doc := "name: t\nsteps:\n"
	for i, s := range steps {
		doc += fmt.Sprintf("  - action: %s\n    name: s%d\n    params:\n      command: %q\n", s.action, i+1, s.command)
	}
	path := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestForkingStepDoesNotBlock(t *testing.T) {
	script := filepath.Join(t.TempDir(), "step.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 2 &\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wf := loadSteps(t, shell("sh "+script))

	ran := make(chan error, 1)
	go func() { ran <- wf.run(context.Background()) }()
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
	wf := loadSteps(t, shell("/usr/bin/touch "+first), shell("/usr/bin/false"), shell("/usr/bin/touch "+third))

	if err := wf.run(context.Background()); err == nil {
		t.Fatal("a failing step must stop the workflow")
	}
	if _, err := os.Stat(first); err != nil {
		t.Error("the first step must have run")
	}
	if _, err := os.Stat(third); err == nil {
		t.Error("the step after the failure must not have run")
	}
}
