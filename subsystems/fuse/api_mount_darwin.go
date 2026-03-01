//go:build darwin

package fuse

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// Mount state for the active NFS-backed mount on macOS.
var (
	activeNFS       *NFSServer
	activeClient    *Client
	activeMountPath string
)

// ActionMountRemote creates a local mount at ~/sessions/<session_id>/ that
// proxies filesystem operations to a remote FUSE TCP server at serverAddr.
// On macOS this works by running a local NFSv3 proxy (go-nfs) and mounting
// it via the built-in mount_nfs command.
// Returns the local mount path and NFS port.
func ActionMountRemote(params map[string]any) (map[string]any, error) {
	sessionID, _ := params["session_id"].(string)
	if sessionID == "" {
		return nil, fmt.Errorf("fuse.mount_remote: session_id is required")
	}
	serverAddr, _ := params["server_addr"].(string)
	if serverAddr == "" {
		return nil, fmt.Errorf("fuse.mount_remote: server_addr is required")
	}

	// 1. Connect to the remote FUSE TCP server.
	client, err := NewClient(serverAddr)
	if err != nil {
		return nil, fmt.Errorf("fuse.mount_remote: connect to %s: %w", serverAddr, err)
	}

	// 2. Start a local NFSv3 proxy backed by the FUSE client.
	nfsSrv := NewNFSServer(client)
	if err := nfsSrv.Start(); err != nil {
		client.Close()
		return nil, fmt.Errorf("fuse.mount_remote: start NFS server: %w", err)
	}

	// 3. Create the mount point directory.
	home, err := os.UserHomeDir()
	if err != nil {
		nfsSrv.Stop()
		client.Close()
		return nil, fmt.Errorf("fuse.mount_remote: get home dir: %w", err)
	}
	mp := filepath.Join(home, "sessions", sessionID)
	if err := os.MkdirAll(mp, 0o755); err != nil {
		nfsSrv.Stop()
		client.Close()
		return nil, fmt.Errorf("fuse.mount_remote: create mount point %q: %w", mp, err)
	}

	// 4. Mount via the built-in mount_nfs.
	nfsPort := fmt.Sprintf("%d", nfsSrv.Port())
	mountSrc := "localhost:/"
	cmd := exec.Command("mount_nfs",
		"-o", fmt.Sprintf("vers=3,tcp,port=%s,mountport=%s,nolocks,noresvport,locallocks", nfsPort, nfsPort),
		mountSrc, mp,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		nfsSrv.Stop()
		client.Close()
		return nil, fmt.Errorf("fuse.mount_remote: mount_nfs: %s: %w", string(out), err)
	}

	// 5. Store state.
	mu.Lock()
	activeNFS = nfsSrv
	activeClient = client
	activeMountPath = mp
	mu.Unlock()

	log.Printf("fuse: mounted %s at %s via NFS port %s", serverAddr, mp, nfsPort)
	return map[string]any{
		"mount_path": mp,
		"nfs_port":   nfsSrv.Port(),
	}, nil
}

// isMountActive reports whether an NFS mount is currently active.
func isMountActive() bool {
	return activeNFS != nil
}

// mountPoint returns the active mount's local path, or "" if none.
func mountPoint() string {
	return activeMountPath
}

// cleanupMount unmounts and tears down the NFS proxy and FUSE client.
func cleanupMount() {
	if activeMountPath != "" {
		cmd := exec.Command("umount", activeMountPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("fuse: umount %s: %s: %v", activeMountPath, string(out), err)
		}
		activeMountPath = ""
	}
	if activeNFS != nil {
		activeNFS.Stop()
		activeNFS = nil
	}
	if activeClient != nil {
		activeClient.Close()
		activeClient = nil
	}
}
