// Package httpapi is linkspan's HTTP surface: the route table, and the
// listeners it is served on.
package httpapi

import (
	"log"
	"net"
	"net/http"
	"os"

	"github.com/cyber-shuttle/linkspan/subsystems/metrics"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/cyber-shuttle/linkspan/utils"
)

// cs-bridge calls these exact paths; renaming one is an API break.
func Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", health)
	mux.HandleFunc("GET /api/v1/metrics", metrics.Handler)
	mux.HandleFunc("GET /api/v1/vscode/sessions", vscode.ListSessions)
	mux.HandleFunc("POST /api/v1/vscode/sessions", vscode.CreateSession)
	return mux
}

func health(w http.ResponseWriter, r *http.Request) {
	utils.RespondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
