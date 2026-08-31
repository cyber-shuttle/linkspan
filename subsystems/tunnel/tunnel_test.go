package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadWritesTheWholeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("binary"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "devtunnel")
	if err := download(context.Background(), dst, srv.URL); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "binary" {
		t.Fatalf("got %q (%v), want %q", b, err, "binary")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("expected only the binary, got %d entries", len(entries))
	}
	// The rename publishes it, so anything after the rename may never happen.
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("published binary is not executable: mode %v", info.Mode().Perm())
	}
}

// A failed transfer must leave nothing where the next run would execute it.
func TestDownloadLeavesNoPartialBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := download(context.Background(), filepath.Join(dir, "devtunnel"), srv.URL); err == nil {
		t.Fatal("expected an error on a non-200 response")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("expected an empty dir, got %d entries", len(entries))
	}
}

func TestDownloadHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("binary"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	if err := download(ctx, filepath.Join(dir, "devtunnel"), srv.URL); err == nil {
		t.Fatal("expected a cancelled context to fail the download")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("expected an empty dir, got %d entries", len(entries))
	}
}

// Under -race, this catches an unguarded buffer.
func TestOutputIsReadableWhileTheProcessRuns(t *testing.T) {
	p, err := start(exec.Command("sh", "-c", "echo first; sleep 0.3; echo second"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.kill)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.String(), "second") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never arrived: %q", p.String())
}

func TestExitedAndKill(t *testing.T) {
	p, err := start(exec.Command("sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	if p.exited() {
		t.Fatal("a just-started process should not report exited")
	}
	p.kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !p.exited() {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.exited() {
		t.Fatal("process never reported exited after kill")
	}
	(*process)(nil).kill() // a relay that was never started
}

func TestOutputIsCapped(t *testing.T) {
	p := &process{done: make(chan struct{})}
	for range 4 {
		n, err := p.Write(make([]byte, outputLimit/2))
		if n != outputLimit/2 || err != nil {
			t.Fatalf("Write reported (%d, %v); it must never fail the child's write", n, err)
		}
	}
	if got := len(p.String()); got != outputLimit {
		t.Fatalf("buffered %d bytes, want it capped at %d", got, outputLimit)
	}
}

// A fake devtunnel CLI that never reports ready and heartbeats into the file
// named by its tunnel id, so a relay left running is visible after the fact.
func fakeDevtunnel(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".linkspan", "bin", "devtunnel")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile :; do echo . >> \"$2\"; sleep 0.02; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture we execute ourselves
		t.Fatal(err)
	}
	t.Cleanup(func() {
		relayMu.Lock()
		relay, stopped = nil, false
		relayMu.Unlock()
	})
}

func beats(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(b)
}

// Shutdown landing during bring-up: main os.Exits the moment StopRelay returns,
// so a relay spawned while StopRelay is in progress is never killed. The start
// must therefore not happen while relayMu is held.
func TestBringUpDoesNotSpawnWhileStopRelayHoldsTheLock(t *testing.T) {
	fakeDevtunnel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beat := filepath.Join(t.TempDir(), "beat")

	relayMu.Lock()
	errc := make(chan error, 1)
	go func() { _, err := hostOnce(ctx, beat, "", "token"); errc <- err }()
	time.Sleep(200 * time.Millisecond) // far longer than a fork/exec
	spawned := beats(t, beat) > 0
	relayMu.Unlock()

	StopRelay()
	if spawned {
		t.Fatal("bring-up spawned a relay while the relay lock was held: StopRelay cannot kill it")
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected the bring-up to fail once shutdown had started")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bring-up never returned after StopRelay")
	}
	before := beats(t, beat)
	time.Sleep(200 * time.Millisecond)
	if after := beats(t, beat); after != before {
		t.Fatalf("relay still running after StopRelay: heartbeat grew %d -> %d", before, after)
	}
}

func TestBringUpAfterStopRelaySpawnsNothing(t *testing.T) {
	fakeDevtunnel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beat := filepath.Join(t.TempDir(), "beat")

	StopRelay()
	go func() { _, _ = hostOnce(ctx, beat, "", "token") }()
	time.Sleep(300 * time.Millisecond)
	if beats(t, beat) > 0 {
		t.Fatal("a bring-up that started after StopRelay spawned a relay nothing will kill")
	}
}
