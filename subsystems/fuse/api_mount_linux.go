//go:build linux

package fuse

import (
	"fmt"
	"os"
	"path/filepath"
)

// activeMount holds the currently active FUSE mount, if any.
var activeMount *Mount

// ActionMountRemote creates a FUSE mount at ~/sessions/<session_id>/ that
// proxies filesystem operations to a remote FUSE TCP server at serverAddr.
// Returns the local mount path as "mount_path".
func ActionMountRemote(params map[string]any) (map[string]any, error) {
	sessionID, _ := params["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("fuse.mount_remote: session_id is required")
	}
	serverAddr, _ := params["server_addr"].(string)
	if serverAddr == "" {
		return nil, fmt.Errorf("fuse.mount_remote: server_addr is required")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("fuse.mount_remote: get home dir: %w", err)
	}
	mp := filepath.Join(home, "sessions", sessionID)

	mount := NewMount(mp, serverAddr)
	if err := mount.Start(); err != nil {
		return nil, fmt.Errorf("fuse.mount_remote: %w", err)
	}

	mu.Lock()
	activeMount = mount
	mu.Unlock()

	return map[string]any{"mount_path": mp}, nil
}

// isMountActive reports whether a FUSE mount is currently active.
// Called with mu held by HandleGetStatus / Cleanup.
func isMountActive() bool {
	return activeMount != nil
}

// mountPoint returns the active mount's local path, or "" if none.
// Called with mu held by HandleGetStatus.
func mountPoint() string {
	if activeMount != nil {
		return activeMount.MountPoint()
	}
	return ""
}

// cleanupMount stops and clears the active mount, if any.
// Called with mu held by Cleanup.
func cleanupMount() {
	if activeMount != nil {
		activeMount.Stop()
		activeMount = nil
	}
}
