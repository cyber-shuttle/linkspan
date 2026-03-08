package mount

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// OverlayMount holds the state for a single overlay filesystem setup.
type OverlayMount struct {
	SessionID string
	SourceDir string // not used as a mount point anymore; kept for compatibility
	CacheDir  string // local cache (upper layer)
	WorkDir   string // unused with Go FUSE overlay; kept for compatibility
	MergedDir string // final merged FUSE view
	overlayFS *OverlayFS
}

// SetupOverlay creates a userspace overlay filesystem using a single Go FUSE
// mount. The lower layer is the remote workspace accessed over SFTP (via the
// tunnel), and the upper layer is the local mutagen-warmed cache. No root
// privileges are required.
func SetupOverlay(sessionID string, localSshPort int, localWorkspace string) (*OverlayMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("overlay: get home dir: %w", err)
	}

	m := &OverlayMount{
		SessionID: sessionID,
		CacheDir:  filepath.Join(home, "sessions", sessionID),
		MergedDir: filepath.Join(home, "overlay", sessionID),
	}

	for _, dir := range []string{m.CacheDir, m.MergedDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("overlay: mkdir %s: %w", dir, err)
		}
	}

	sshAddr := fmt.Sprintf("localhost:%d", localSshPort)
	ofs, err := MountOverlayFS(sshAddr, localWorkspace, m.CacheDir, m.MergedDir)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}
	m.overlayFS = ofs
	log.Printf("[overlay] merged at %s (upper=%s, lower=sftp://localhost:%d%s)", m.MergedDir, m.CacheDir, localSshPort, localWorkspace)

	return m, nil
}

// Teardown unmounts the overlay FUSE filesystem.
func (m *OverlayMount) Teardown() {
	if m.overlayFS != nil {
		m.overlayFS.Unmount()
		log.Printf("[overlay] unmounted for %s", m.SessionID)
	}
}
