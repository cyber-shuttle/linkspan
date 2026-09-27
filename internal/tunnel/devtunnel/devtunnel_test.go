// Tests for the Dev Tunnel host process's lifecycle under StopAll. The heartbeat fake appends to a file named by its
// Dev Tunnel id, so a host process left running is visible.
//
//	newTunnel  Installs a fake CLI under a temporary HOME, $2 being the qualified id.
//	beatCount
//	TestOutputKeepsTheTail  The last 64KB must be kept.
//	TestStopAllKillsTheHost, TestHostExitReturnsItsOutput
package devtunnel

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

func newTunnel(t *testing.T, id, script string) *Tunnel {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".cybershuttle", "bin", "devtunnel")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Tunnel{id: id, cluster: "c", token: "token"}
}

func beatCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(b)
}

func TestOutputKeepsTheTail(t *testing.T) {
	o := &output{}
	_, _ = o.Write(bytes.Repeat([]byte("x"), 70<<10))
	_, _ = o.Write([]byte("tail\n"))
	if s := o.String(); len(s) != 64<<10 || !strings.HasSuffix(s, "tail\n") {
		t.Fatalf("captured %d bytes ending %q; want the last 64KB", len(s), s[len(s)-5:])
	}
}

func TestStopAllKillsTheHost(t *testing.T) {
	id := filepath.Join(t.TempDir(), "beat")
	tn, beat := newTunnel(t, id, "#!/bin/sh\nwhile :; do echo . >> \"$2\"; /bin/sleep 0.02; done\n"), id+".c"
	_, _ = (&tasks.Task{Kind: "devtunnel", Run: tn.Host}).Start()

	for deadline := time.Now().Add(5 * time.Second); beatCount(t, beat) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fake host process never started")
		}
	}

	tasks.StopAll()
	before := beatCount(t, beat)
	time.Sleep(200 * time.Millisecond)
	if now := beatCount(t, beat); now != before {
		t.Fatalf("host process still running after StopAll: heartbeat grew %d -> %d", before, now)
	}
}

func TestHostExitReturnsItsOutput(t *testing.T) {
	tn := newTunnel(t, "t", "#!/bin/sh\necho hosting\n")
	exited := make(chan error, 1)
	go func() { exited <- tn.Host(context.Background()) }()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(err.Error(), "host process exited") {
			t.Fatalf("want a host-exited error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the task never returned after the host process died")
	}
}
