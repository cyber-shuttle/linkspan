// Package httpapi serves the four /api/v1 routes (docs/COMPATIBILITY.md) and
// binds their listeners. Requests carry no credential; reaching a listener is
// the authorisation (SECURITY.md).
//
//	HttpAPI       Its Start serves the API on the bound listener under procmgr as
//	              http-<network>, with bodies capped at 64KB; a larger body
//	              answers 413.
//	respond       It is the only writer. A handler returns a status, a body and
//	              an error message; a non-empty message is sent as the error
//	              shape.
//	health, collectMetrics
//	listSessions  It lists by id, so two calls agree on the order.
//	startSession  It parses the key and starts one SSH server for it. A key
//	              carrying authorized_keys options is refused, because the
//	              server would ignore them.
//	mux           It is the route table.
//	New           It binds one listener. A stale socket at a unix path is
//	              unlinked first; any other file there fails the bind. The
//	              socket is set to mode 0600. A failed bind is returned, not
//	              retried.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
	"github.com/cyber-shuttle/linkspan/subsystems/metrics"
	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	gossh "golang.org/x/crypto/ssh"
)

type HttpAPI struct {
	network string
	ln      net.Listener
}

func respond(h func(*http.Request) (int, any, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, errMsg := h(r)
		if errMsg != "" {
			body = map[string]string{"error": errMsg}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func health(*http.Request) (int, any, string) {
	return http.StatusOK, map[string]string{"status": "ok"}, ""
}

func collectMetrics(*http.Request) (int, any, string) {
	return http.StatusOK, metrics.Collect(), ""
}

func listSessions(*http.Request) (int, any, string) {
	running := procmgr.Running(procmgr.KindSSHD)
	slices.SortFunc(running, func(a, b *procmgr.Process) int { return strings.Compare(a.ID, b.ID) })
	sessions := make([]map[string]string, 0, len(running))
	for _, p := range running {
		sessions = append(sessions, map[string]string{"id": p.ID, "state": "running", "addr": p.Addr})
	}
	return http.StatusOK, sessions, ""
}

func startSession(r *http.Request) (int, any, string) {
	var req struct {
		AuthorizedKey string `json:"authorized_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return http.StatusRequestEntityTooLarge, nil, err.Error()
		}
		return http.StatusBadRequest, nil, "invalid JSON: " + err.Error()
	}
	key, _, options, _, err := gossh.ParseAuthorizedKey([]byte(req.AuthorizedKey))
	if err != nil {
		return http.StatusBadRequest, nil, "authorized_key is missing or invalid"
	}
	if len(options) > 0 {
		return http.StatusBadRequest, nil, "authorized_key options are not supported"
	}
	s, err := sshd.New(key)
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	s.Start()
	return http.StatusCreated, map[string]any{"id": s.ID, "bind_port": s.Port}, ""
}

func mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/health", respond(health))
	m.HandleFunc("GET /api/v1/metrics", respond(collectMetrics))
	m.HandleFunc("GET /api/v1/vscode/sessions", respond(listSessions))
	m.HandleFunc("POST /api/v1/vscode/sessions", respond(startSession))
	return m
}

func New(network, addr string) (*HttpAPI, error) {
	if network == "unix" {
		if fi, err := os.Lstat(addr); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(addr)
		}
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	if network == "unix" {
		if err := os.Chmod(addr, 0o600); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("httpapi: %w", err)
		}
	}
	return &HttpAPI{network: network, ln: ln}, nil
}

func (a *HttpAPI) Start() {
	log.Printf("httpapi: listening on %s", a.ln.Addr())
	srv := &http.Server{Handler: http.MaxBytesHandler(mux(), 64<<10), ReadHeaderTimeout: 10 * time.Second}
	procmgr.Start(procmgr.KindHTTP, "http-"+a.network, a.ln.Addr().String(), procmgr.Serve(a.ln, srv))
}
