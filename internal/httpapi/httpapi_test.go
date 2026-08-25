package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	gossh "golang.org/x/crypto/ssh"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "linkspan.sock")
	os.WriteFile(sock, []byte("stale"), 0o600) // a prior run's leftover, which ListenUnix must clear

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})}
	if err := ListenUnix(srv, sock); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Get("http://unix/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// The four paths cs-bridge calls. Renaming one is an API break.
func TestMuxRoutesTheConsumerContract(t *testing.T) {
	mux := Mux()
	for _, r := range [][2]string{
		{"GET", "/api/v1/health"},
		{"GET", "/api/v1/metrics"},
		{"GET", "/api/v1/vscode/sessions"},
		{"POST", "/api/v1/vscode/sessions"},
	} {
		if _, pattern := mux.Handler(httptest.NewRequest(r[0], r[1], nil)); pattern == "" {
			t.Errorf("%s %s is not routed", r[0], r[1])
		}
	}
}

// cs-bridge parses {"id":"s-<port>","bind_port":<port>} in linkspanSupport.ts;
// renaming a field, reshaping the id, or not answering 201 breaks it.
func TestCreateSessionResponseShape(t *testing.T) {
	t.Cleanup(sshd.StopAll)

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"authorized_key":` + strconv.Quote(string(gossh.MarshalAuthorizedKey(signer.PublicKey()))) + `}`

	rec := httptest.NewRecorder()
	Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/vscode/sessions", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var got struct {
		ID       string `json:"id"`
		BindPort int32  `json:"bind_port"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not the documented object: %v (%s)", err, rec.Body)
	}
	if got.BindPort == 0 {
		t.Fatalf("bind_port missing or zero: %s", rec.Body)
	}
	if want := "s-" + strconv.Itoa(int(got.BindPort)); got.ID != want {
		t.Fatalf("id = %q, want %q", got.ID, want)
	}
}

func TestCreateSessionRejectsAnUnusableKey(t *testing.T) {
	rec := httptest.NewRecorder()
	Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/vscode/sessions",
		strings.NewReader(`{"authorized_key":"not-a-key"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Unauthenticated routes behind a socket cs-bridge puts in a shared directory.
func TestListenUnixSocketIsNotGroupOrWorldAccessible(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock") // not t.TempDir: macOS caps socket paths at 104 chars
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "linkspan.sock")
	srv := &http.Server{Handler: Mux()}
	if err := ListenUnix(srv, sock); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode %v; group and other must have no access", perm)
	}
}

// cs-bridge reads these bodies, not just the status codes.
func TestContractResponseBodies(t *testing.T) {
	t.Cleanup(sshd.StopAll)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		return rec
	}

	if got := strings.TrimSpace(get("/api/v1/health").Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("health body = %s, want {\"status\":\"ok\"}", got)
	}

	// A JSON object, so absent sources are omitted rather than sent as zeroes.
	var snap map[string]any
	if err := json.Unmarshal(get("/api/v1/metrics").Body.Bytes(), &snap); err != nil {
		t.Fatalf("metrics body is not an object: %v (%s)", err, get("/api/v1/metrics").Body)
	}

	_, _, err := startTestSession(t)
	if err != nil {
		t.Fatal(err)
	}
	var sessions []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Addr  string `json:"addr"`
	}
	if err := json.Unmarshal(get("/api/v1/vscode/sessions").Body.Bytes(), &sessions); err != nil {
		t.Fatalf("sessions body is not the documented array: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("no sessions listed after creating one")
	}
	s := sessions[0]
	if s.ID == "" || s.Addr == "" {
		t.Fatalf("id/addr missing from %+v", s)
	}
	if s.State != "running" {
		t.Fatalf("state = %q, want %q -- cs-bridge compares against this literal", s.State, "running")
	}
}

func startTestSession(t *testing.T) (string, int32, error) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", 0, err
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		return "", 0, err
	}
	body := `{"authorized_key":` + strconv.Quote(string(gossh.MarshalAuthorizedKey(signer.PublicKey()))) + `}`
	rec := httptest.NewRecorder()
	Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/vscode/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		return "", 0, fmt.Errorf("create = %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		ID       string `json:"id"`
		BindPort int32  `json:"bind_port"`
	}
	return out.ID, out.BindPort, json.Unmarshal(rec.Body.Bytes(), &out)
}
