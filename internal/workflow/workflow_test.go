package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, doc string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "step.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// cs-control's provisioning ends in `setsid --fork python -m jupyter_server`,
// which leaves a grandchild holding the inherited stdout.
func TestAStepThatForksDoesNotBlockTheWorkflow(t *testing.T) {
	wf := load(t, "name: d\nsteps:\n  - action: shell.exec\n    name: fork\n    params:\n      command: \"/bin/sh "+script(t, "/bin/sleep 3 &")+"\"\n")

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), wf) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("step failed: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		// The child sleeps 3s; a pipe would make Run wait for it.
		t.Fatal("Run blocked on a step that forked a long-lived child")
	}
}

func TestStepsRunInOrderAndStopAtTheFirstFailure(t *testing.T) {
	dir := t.TempDir()
	first, third := filepath.Join(dir, "first"), filepath.Join(dir, "third")
	wf := load(t, "name: o\nsteps:\n"+
		"  - action: shell.exec\n    name: one\n    params:\n      command: \"/usr/bin/touch "+first+"\"\n"+
		"  - action: shell.exec\n    name: two\n    params:\n      command: \"/usr/bin/false\"\n"+
		"  - action: shell.exec\n    name: three\n    params:\n      command: \"/usr/bin/touch "+third+"\"\n")

	if err := Run(context.Background(), wf); err == nil {
		t.Fatal("expected the failing step to stop the workflow")
	}
	if _, err := os.Stat(first); err != nil {
		t.Error("the first step should have run")
	}
	if _, err := os.Stat(third); err == nil {
		t.Error("the step after the failure should not have run")
	}
}

func TestRejectsAnUnknownAction(t *testing.T) {
	wf := load(t, "name: u\nsteps:\n  - action: shell.evaluate\n    name: nope\n    params:\n      command: \"/usr/bin/true\"\n")
	if err := Run(context.Background(), wf); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("got %v, want an unknown-action error", err)
	}
}

func TestRejectsAnEmptyCommand(t *testing.T) {
	wf := load(t, "name: e\nsteps:\n  - action: shell.exec\n    name: blank\n    params:\n      command: \"\"\n")
	if err := Run(context.Background(), wf); err == nil {
		t.Fatal("expected an empty command to be rejected")
	}
}

func TestACancelledContextStopsBeforeTheNextStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wf := load(t, "name: c\nsteps:\n  - action: shell.exec\n    name: one\n    params:\n      command: \"/usr/bin/true\"\n")
	if err := Run(ctx, wf); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("got %v, want a cancellation error", err)
	}
}

func TestLoadFile(t *testing.T) {
	wf := load(t, "name: demo\nsteps:\n  - action: shell.exec\n    name: greet\n    params:\n      command: \"echo hi\"\n")
	if wf.Name != "demo" || len(wf.Steps) != 1 || wf.Steps[0].Params.Command != "echo hi" {
		t.Fatalf("parsed %+v", wf)
	}
	if _, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
