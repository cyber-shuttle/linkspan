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
	// Executable the moment it appears: the rename publishes it, so anything
	// that happens after the rename may never happen at all.
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

// The ready-wait reads the output while the child is still writing it, so an
// unguarded buffer races. Under -race this is what catches that.
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

// The relay runs for the whole allocation inside a memory-capped cgroup, and
// nothing reads its output after the ready-wait.
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
