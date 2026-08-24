package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "linkspan.sock")
	os.WriteFile(sock, []byte("stale"), 0o600) // prior-run leftover listenUnix must clear

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

// The four paths both consumers call. Renaming one is an API break.
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
