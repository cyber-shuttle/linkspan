package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListenUnix verifies the agent can be reached over a unix domain socket,
// and that a stale socket file left by a prior run is removed before binding.
func TestListenUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "linkspan.sock")

	// Simulate a leaked socket file from a SIGKILLed run.
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Handler: mux}
	if _, err := listenUnix(srv, sock); err != nil {
		t.Fatalf("listenUnix over stale file: %v", err)
	}
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}

	// The serve goroutine may not be ready on the first request.
	var resp *http.Response
	var err error
	for range 100 {
		resp, err = client.Get("http://unix/health")
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
