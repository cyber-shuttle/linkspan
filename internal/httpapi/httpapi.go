// Package httpapi is linkspan's HTTP surface: the route table and the handlers
// behind it. It is the only package that serves HTTP -- the subsystems report
// data and know nothing about requests. main owns the http.Server and the TCP
// listener; ListenUnix here adds the optional unix socket.
package httpapi

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/cyber-shuttle/linkspan/subsystems/metrics"
	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	gossh "golang.org/x/crypto/ssh"
)

// An ssh public key is well under a kilobyte. The cap stops a large body on a
// tunnel-reachable endpoint from being read into memory.
const maxRequestBytes = 64 << 10

// cs-bridge calls these exact paths; renaming one is an API break.
func Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", health)
	mux.HandleFunc("GET /api/v1/metrics", jobMetrics)
	mux.HandleFunc("GET /api/v1/vscode/sessions", listSessions)
	mux.HandleFunc("POST /api/v1/vscode/sessions", createSession)
	return mux
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func jobMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, metrics.Read(r.Context()))
}

func listSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, sshd.Statuses())
}

type sessionRequest struct {
	AuthorizedKey string `json:"authorized_key"` // the private half never leaves the caller
}

type sessionResponse struct {
	ID       string `json:"id"`
	BindPort int32  `json:"bind_port"`
}

func createSession(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	// net/http closes the body itself.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	authorized, _, _, _, err := gossh.ParseAuthorizedKey([]byte(req.AuthorizedKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, "authorized_key is missing or invalid")
		return
	}

	id, port, err := sshd.Start(authorized)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sessionResponse{ID: id, BindPort: int32(port)})
}

// ListenUnix serves srv on a unix socket in the background, reachable in-cluster
// via `srun --jobid` with no TCP port.
func ListenUnix(srv *http.Server, path string) error {
	os.Remove(path) // a stale socket from a prior run blocks the bind
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("unix socket server error: %v", err)
		}
	}()
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
