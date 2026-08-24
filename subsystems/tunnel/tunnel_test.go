package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
