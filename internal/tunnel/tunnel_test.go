// Tests for the relay's lifecycle under StopAll and for port publishing. The heartbeat fake appends to a file named by
// its tunnel id, so a relay left running is visible.
//
//	newTunnel  Installs a fake CLI under a temporary HOME, $2 being the qualified id, and makes it the active tunnel.
//	beatCount
//	TestOutputKeepsTheTail  The last 64KB must be kept.
//	TestStopAllKillsTheRelay, TestRelayExitEndsTheTask
//	TestReadyFollowsTheRelay  Ready is closed by the relay's ready line, and already closed with no tunnel.
//	TestPublish        A port is PUT with the host token, anonymous when asked, and DELETEd when its context ends; a
//	                   failure must not name the token.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	tn, err := New(id, "c", "token")
	if err != nil {
		t.Fatal(err)
	}
	return tn
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
	tn := newTunnel(t, "t", "#!/bin/sh\necho 'Ready to accept connections'\n")
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

func TestReadyFollowsTheRelay(t *testing.T) {
	active.Store(nil)
	select {
	case <-Ready():
	default:
		t.Fatal("with no tunnel, Ready must be closed")
	}
	tn := newTunnel(t, "t", "#!/bin/sh\necho 'Ready to accept connections'\n/bin/sleep 30\n")
	t.Cleanup(func() { active.Store(nil) })
	select {
	case <-Ready():
		t.Fatal("Ready closed before the relay ran")
	default:
	}
	_, _ = (&tasks.Task{Kind: "tunnel", Run: tn.Relay}).Start()
	t.Cleanup(tasks.StopAll)
	select {
	case <-Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("Ready never closed after the relay's ready line")
	}
}

func TestPublish(t *testing.T) {
	type call struct{ method, path, auth string }
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path + "?" + r.URL.RawQuery, r.Header.Get("Authorization")})
		mu.Unlock()
		if r.Method == http.MethodPut && !strings.HasSuffix(r.URL.Path, "/ports/4000") {
			http.Error(w, "quota", http.StatusForbidden)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["portNumber"] != float64(4000) || body["protocol"] != "http" {
			t.Errorf("port body = %v", body)
		}
		if _, anonymous := body["accessControl"]; anonymous != (r.Method == http.MethodPut) {
			t.Errorf("PUT must carry the anonymous entry and DELETE none, got %v", body)
		}
	}))
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL + "/{cluster}"
	t.Cleanup(func() { apiBase = old })

	active.Store(nil)
	if err := Publish(context.Background(), 4000, true); err != nil || URL(4000) != "" {
		t.Fatalf("with no active tunnel got %q, %v; want no URL and no error", URL(4000), err)
	}

	_, _ = New("tid", "c", "secret-token")
	t.Cleanup(func() { active.Store(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	if err := Publish(ctx, 4000, true); err != nil || URL(4000) != "https://tid-4000.c.devtunnels.ms" {
		t.Fatalf("Publish = %v, URL = %q", err, URL(4000))
	}
	if want := (call{"PUT", "/c/tunnels/tid/ports/4000?api-version=" + apiVersion, "tunnel secret-token"}); len(calls) != 1 || calls[0] != want {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	if err := Publish(ctx, 4001, false); err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused port must fail naming the status and not the token, got %v", err)
	}
	cancel()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		last := calls[len(calls)-1]
		mu.Unlock()
		if last.method == http.MethodDelete && strings.HasSuffix(last.path, "/ports/4000?api-version="+apiVersion) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the port was not removed when its context ended: %+v", calls)
		}
	}
}
