package fuse

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/cyber-shuttle/linkspan/utils"
)

// Global state for the active TCP FUSE server. Mount state is managed in the
// platform-specific api_mount_*.go files.
var (
	activeServer   *Server
	activeServerLn net.Listener
	mu             sync.Mutex
)

// ActionStartServer starts a FUSE TCP server on a random port, serving
// the given root filesystem path. Returns the port number as "fuse_port".
func ActionStartServer(params map[string]any) (map[string]any, error) {
	rootPath, _ := params["root"].(string)
	if rootPath == "" {
		rootPath = "/"
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("fuse.start_server: listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	srv := NewServer(rootPath)

	mu.Lock()
	activeServer = srv
	activeServerLn = ln
	mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Printf("fuse: TCP server error: %v", err)
		}
	}()
	log.Printf("fuse: TCP server started on port %d (root: %s)", port, rootPath)

	return map[string]any{"fuse_port": port}, nil
}

// HandleGetStatus handles GET /api/v1/fuse/status. It reports whether the
// TCP server and any local FUSE mount are currently active.
func HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	status := map[string]any{
		"server_running": activeServer != nil,
		"mount_active":   isMountActive(),
	}
	if mp := mountPoint(); mp != "" {
		status["mount_path"] = mp
	}
	if activeServerLn != nil {
		status["server_addr"] = activeServerLn.Addr().String()
	}
	utils.RespondJSON(w, http.StatusOK, status)
}

// MountRemoteRequest is the JSON body for POST /api/v1/fuse/mount-remote.
type MountRemoteRequest struct {
	SessionID  string `json:"session_id"`
	ServerAddr string `json:"server_addr"`
}

// HandleMountRemote handles POST /api/v1/fuse/mount-remote.
func HandleMountRemote(w http.ResponseWriter, r *http.Request) {
	var req MountRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	result, err := ActionMountRemote(map[string]any{
		"session_id":  req.SessionID,
		"server_addr": req.ServerAddr,
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusOK, result)
}

// Cleanup stops all active FUSE resources: the local FUSE mount (if any) and
// the TCP server. Safe to call multiple times or if nothing was started.
func Cleanup() {
	mu.Lock()
	defer mu.Unlock()

	cleanupMount()

	if activeServer != nil {
		activeServer.Stop()
		activeServer = nil
	}
	if activeServerLn != nil {
		activeServerLn.Close()
		activeServerLn = nil
	}
	log.Println("fuse: cleanup completed")
}
