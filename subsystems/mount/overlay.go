package mount

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// OverlayMount holds the state for a single overlay filesystem setup.
type OverlayMount struct {
	SessionID    string
	SourceDir    string // sshfs mount of local workspace (lower)
	CacheDir     string // mutagen-warmed cache (upper)
	WorkDir      string // overlayfs workdir
	MergedDir    string // final merged view
	SshfsCmdID   string // process manager ID for sshfs
	OverlayCmdID string // process manager ID for fuse-overlayfs
}

// SetupOverlay creates the overlay filesystem:
// 1. sshfs mounts the local workspace via tunnel
// 2. fuse-overlayfs merges lower (sshfs) + upper (cache)
func SetupOverlay(sessionID string, localSshPort int, localWorkspace string) (*OverlayMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("overlay: get home dir: %w", err)
	}

	m := &OverlayMount{
		SessionID: sessionID,
		SourceDir: filepath.Join(os.TempDir(), fmt.Sprintf("cs-source-%s", sessionID)),
		CacheDir:  filepath.Join(home, "sessions", sessionID),
		WorkDir:   filepath.Join(home, "sessions", sessionID, ".overlay-work"),
		MergedDir: filepath.Join(home, "overlay", sessionID),
	}

	// Create all directories
	for _, dir := range []string{m.SourceDir, m.CacheDir, m.WorkDir, m.MergedDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("overlay: mkdir %s: %w", dir, err)
		}
	}

	// 1. sshfs mount local workspace via tunnel
	sshfsArgs := []string{
		fmt.Sprintf("localhost:%s", localWorkspace),
		m.SourceDir,
		"-p", fmt.Sprintf("%d", localSshPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}

	sshfsCmd := exec.Command("sshfs", sshfsArgs...)
	sshfsCmdID, err := pm.GlobalProcessManager.Start(sshfsCmd)
	if err != nil {
		return nil, fmt.Errorf("overlay: start sshfs: %w", err)
	}
	m.SshfsCmdID = sshfsCmdID
	log.Printf("[overlay] sshfs mounted %s on port %d → %s", localWorkspace, localSshPort, m.SourceDir)

	// 2. fuse-overlayfs
	overlayArgs := []string{
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", m.SourceDir, m.CacheDir, m.WorkDir),
		m.MergedDir,
	}

	overlayCmd := exec.Command("fuse-overlayfs", overlayArgs...)
	overlayCmdID, err := pm.GlobalProcessManager.Start(overlayCmd)
	if err != nil {
		// Clean up sshfs on failure
		_ = pm.GlobalProcessManager.Kill(sshfsCmdID)
		return nil, fmt.Errorf("overlay: start fuse-overlayfs: %w", err)
	}
	m.OverlayCmdID = overlayCmdID
	log.Printf("[overlay] fuse-overlayfs merged at %s (lower=%s, upper=%s)", m.MergedDir, m.SourceDir, m.CacheDir)

	return m, nil
}

// Teardown unmounts the overlay and sshfs.
func (m *OverlayMount) Teardown() {
	if m.OverlayCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(m.OverlayCmdID)
		log.Printf("[overlay] stopped fuse-overlayfs for %s", m.SessionID)
	}
	// fusermount -u to cleanly unmount
	_ = exec.Command("fusermount", "-u", m.MergedDir).Run()
	_ = exec.Command("fusermount", "-u", m.SourceDir).Run()

	if m.SshfsCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(m.SshfsCmdID)
		log.Printf("[overlay] stopped sshfs for %s", m.SessionID)
	}
}
