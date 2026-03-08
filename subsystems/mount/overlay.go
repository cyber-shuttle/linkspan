package mount

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// activeOverlay tracks the current overlay mount for cleanup on shutdown.
var activeOverlay *OverlayMount

// CleanupAll unmounts any active overlay and cleans stale mounts.
// Called during graceful shutdown.
func CleanupAll() {
	if activeOverlay != nil {
		activeOverlay.Teardown()
		activeOverlay = nil
	}
	// Also clean any orphaned mounts from previous runs
	home, err := os.UserHomeDir()
	if err == nil {
		cleanupStaleMounts(filepath.Join(home, "overlay"))
	}
}

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

	// Clean up any stale FUSE mounts under ~/overlay/ before proceeding
	cleanupStaleMounts(filepath.Join(home, "overlay"))

	for _, dir := range []string{m.CacheDir, m.MergedDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("overlay: mkdir %s: %w", dir, err)
		}
	}

	sshAddr := fmt.Sprintf("localhost:%d", localSshPort)
	ofs, err := MountOverlayFS(sshAddr, localWorkspace, m.CacheDir, m.MergedDir)
	if err != nil {
		// Clean up the mount point if FUSE mount failed partway
		defer forceUnmount(m.MergedDir)
		return nil, fmt.Errorf("overlay: %w", err)
	}
	m.overlayFS = ofs
	activeOverlay = m
	log.Printf("[overlay] merged at %s (upper=%s, lower=sftp://localhost:%d%s)", m.MergedDir, m.CacheDir, localSshPort, localWorkspace)

	return m, nil
}

// Teardown unmounts the overlay FUSE filesystem with failsafe fallbacks.
func (m *OverlayMount) Teardown() {
	if m.overlayFS != nil {
		m.overlayFS.Unmount()
		log.Printf("[overlay] unmounted for %s", m.SessionID)
	}
	// Failsafe: try fusermount even if the Go unmount failed or overlayFS was nil
	forceUnmount(m.MergedDir)
}

// cleanupStaleMounts unmounts all stale FUSE mounts under the given directory
// by scanning /proc/mounts (or mount output) for linkspan-overlay entries.
func cleanupStaleMounts(overlayDir string) {
	// Strategy 1: parse /proc/mounts for our FUSE mounts
	if data, err := os.ReadFile("/proc/mounts"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[1], overlayDir) {
				log.Printf("[overlay] cleaning stale mount: %s", fields[1])
				forceUnmount(fields[1])
			}
		}
		return
	}

	// Strategy 2: parse `mount` output (macOS / fallback)
	out, err := exec.Command("mount").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "device on /path type fuse.overlayfs ..."
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "on" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], overlayDir) {
				log.Printf("[overlay] cleaning stale mount: %s", fields[i+1])
				forceUnmount(fields[i+1])
			}
		}
	}
}

// forceUnmount tries multiple strategies to unmount a path.
func forceUnmount(path string) {
	// 1. fusermount -u (standard FUSE unmount)
	if err := exec.Command("fusermount", "-u", path).Run(); err == nil {
		return
	}
	// 2. fusermount -uz (lazy unmount)
	if err := exec.Command("fusermount", "-uz", path).Run(); err == nil {
		return
	}
	// 3. umount (may work if we have permissions)
	if err := exec.Command("umount", path).Run(); err == nil {
		return
	}
	// 4. umount -l (lazy)
	_ = exec.Command("umount", "-l", path).Run()
}
