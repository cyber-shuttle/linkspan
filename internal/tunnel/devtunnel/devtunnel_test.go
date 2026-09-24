// Tests for the relay's lifecycle under StopAll. The heartbeat fake appends to a file named by its tunnel id, so a
// relay left running is visible.
//
//	newTunnel  Installs a fake CLI under a temporary HOME, $2 being the qualified id.
//	beatCount
//	TestOutputKeepsTheTail  The last 64KB must be kept.
//	TestStopAllKillsTheRelay, TestRelayExitEndsTheTask
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

func TestStopAllKillsTheRelay(t *testing.T) {
	id := filepath.Join(t.TempDir(), "beat")
	tn, beat := newTunnel(t, id, "#!/bin/sh\nwhile :; do echo . >> \"$2\"; /bin/sleep 0.02; done\n"), id+".c"
	_, _ = (&tasks.Task{Kind: "tunnel", Run: tn.Relay}).Start()

	for deadline := time.Now().Add(5 * time.Second); beatCount(t, beat) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fake relay never started")
		}
	}

	tasks.StopAll()
	before := beatCount(t, beat)
	time.Sleep(200 * time.Millisecond)
	if now := beatCount(t, beat); now != before {
		t.Fatalf("relay still running after StopAll: heartbeat grew %d -> %d", before, now)
	}
}

func TestRelayExitEndsTheTask(t *testing.T) {
	tn := newTunnel(t, "t", "#!/bin/sh\necho hosting\n")
	exited := make(chan error, 1)
	go func() { exited <- tn.Relay(context.Background()) }()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(err.Error(), "relay exited") {
			t.Fatalf("want a relay-exited error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the task never returned after the relay died")
	}
}
