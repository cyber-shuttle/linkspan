// Tests for the /api/v1 surface cs-bridge parses.
//
//	call                             It sends one request through the real route
//	                                 table.
//	createSession                    It posts the test key and returns the id and
//	                                 port of the session created.
//	TestRoutes                       Each frozen pattern must route to a handler
//	                                 registered under exactly that pattern.
//	TestCreateSessionServesOnReturn  The port in the response must already accept
//	                                 connections.
//	TestCreateSessionRejectsBadKey   A key that does not parse, or one carrying
//	                                 options, must answer 400 with the error
//	                                 shape.
//	TestSocketIsOwnerOnly            The socket must be mode 0600, a stale socket
//	                                 must be replaced, and a regular file at the
//	                                 path must fail the bind. The directory
//	                                 avoids t.TempDir because macOS caps socket
//	                                 paths at 104 characters.
//	TestResponseBodies               An empty session list must marshal as [],
//	                                 and only the SSH kind may be listed.
package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
)

const authorizedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH66P8ofDO6v2AaMYZ7JN3lW/m/b32Ab75yYTf3n6NZg test"

func call(method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func createSession(t *testing.T) (string, int) {
	t.Helper()
	t.Cleanup(func() { procmgr.StopAll() })
	rec := call(http.MethodPost, "/api/v1/vscode/sessions", `{"authorized_key": "`+authorizedKey+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		ID       string `json:"id"`
		BindPort int    `json:"bind_port"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the documented object: %v (%s)", err, rec.Body)
	}
	return out.ID, out.BindPort
}

func TestRoutes(t *testing.T) {
	want := []string{
		"GET /api/v1/health",
		"GET /api/v1/metrics",
		"GET /api/v1/vscode/sessions",
		"POST /api/v1/vscode/sessions",
	}
	m := mux()
	for _, pattern := range want {
		method, path, _ := strings.Cut(pattern, " ")
		if _, routed := m.Handler(httptest.NewRequest(method, path, nil)); routed != pattern {
			t.Errorf("%s routes as %q; the surface cs-bridge ships against changed", pattern, routed)
		}
	}
}

func TestCreateSessionServesOnReturn(t *testing.T) {
	id, port := createSession(t)
	if port == 0 {
		t.Fatal("bind_port missing or zero")
	}
	if want := "s-" + strconv.Itoa(port); id != want {
		t.Fatalf("id = %q, want %q", id, want)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("nothing accepting on bind_port when the response was written: %v", err)
	}
	_ = c.Close()
}

func TestCreateSessionRejectsBadKey(t *testing.T) {
	for name, body := range map[string]string{
		"unparsable":   `{"authorized_key":"not-a-key"}`,
		"with options": `{"authorized_key":"from=\"10.0.0.1\" ` + authorizedKey + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := call(http.MethodPost, "/api/v1/vscode/sessions", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Fatalf("error body = %s, want an {\"error\": ...} message", rec.Body)
			}
		})
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "linkspan.sock")
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New("unix", sock); err == nil {
		t.Fatal("a regular file at the socket path was unlinked and replaced")
	}
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	l, err := New("unix", sock)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	l.Start()
	t.Cleanup(func() { procmgr.StopAll() })
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("nothing accepting on the socket after Start: %v", err)
	}
	_ = c.Close()
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode %v; group and other must have no access", perm)
	}
}

func TestResponseBodies(t *testing.T) {
	get := func(path string) *httptest.ResponseRecorder {
		rec := call(http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("GET %s Content-Type = %q, want application/json", path, ct)
		}
		return rec
	}

	if got := strings.TrimSpace(get("/api/v1/health").Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("health body = %s, want {\"status\":\"ok\"}", got)
	}

	metricsBody := get("/api/v1/metrics").Body
	var snap map[string]any
	if err := json.Unmarshal(metricsBody.Bytes(), &snap); err != nil {
		t.Fatalf("metrics body is not an object: %v (%s)", err, metricsBody)
	}

	if body := strings.TrimSpace(get("/api/v1/vscode/sessions").Body.String()); body != "[]" {
		t.Fatalf("an empty sessions list must marshal as [], got %s", body)
	}
	idle := func(ctx context.Context) error { <-ctx.Done(); return nil }
	procmgr.Start(procmgr.KindTunnel, "not-a-session", "relay-addr", idle)
	id, _ := createSession(t)
	var sessions []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Addr  string `json:"addr"`
	}
	if err := json.Unmarshal(get("/api/v1/vscode/sessions").Body.Bytes(), &sessions); err != nil {
		t.Fatalf("sessions body is not the documented array: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != id || sessions[0].Addr == "" {
		t.Fatalf("want the one ssh session %s with its addr, got %+v", id, sessions)
	}
	if sessions[0].State != "running" {
		t.Fatalf("state = %q, want %q -- cs-bridge compares against this literal", sessions[0].State, "running")
	}
}
