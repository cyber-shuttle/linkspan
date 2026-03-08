package mount

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// OverlayMount holds the state for a workspace sync from the local machine.
type OverlayMount struct {
	SessionID string
	SourceDir string // local workspace path on the origin machine
	CacheDir  string // local cache dir on the compute node
	MergedDir string // final workspace view on the compute node
}

// SetupOverlay syncs the local workspace to the compute node via SFTP over
// the devtunnel-forwarded SSH port.  No external tools (sshfs, fuse-overlayfs)
// are needed — everything is done in pure Go.
func SetupOverlay(sessionID string, localSshPort int, localWorkspace string) (*OverlayMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("overlay: get home dir: %w", err)
	}

	m := &OverlayMount{
		SessionID: sessionID,
		SourceDir: localWorkspace,
		CacheDir:  filepath.Join(home, "sessions", sessionID),
		MergedDir: filepath.Join(home, "overlay", sessionID),
	}

	if err := os.MkdirAll(m.MergedDir, 0755); err != nil {
		return nil, fmt.Errorf("overlay: mkdir %s: %w", m.MergedDir, err)
	}

	log.Printf("[overlay] syncing %s from localhost:%d → %s", localWorkspace, localSshPort, m.MergedDir)

	// Connect to the local linkspan's SSH server via devtunnel-forwarded port.
	// The linkspan SSH server accepts any key (no auth required).
	sshConfig := &ssh.ClientConfig{
		User:            "user",
		Auth:            []ssh.AuthMethod{ssh.Password("")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
	}

	addr := fmt.Sprintf("127.0.0.1:%d", localSshPort)
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		// Retry with no-auth in case password is rejected
		sshConfig.Auth = []ssh.AuthMethod{}
		conn, err = ssh.Dial("tcp", addr, sshConfig)
		if err != nil {
			return nil, fmt.Errorf("overlay: ssh connect to %s: %w", addr, err)
		}
	}
	defer conn.Close()

	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, fmt.Errorf("overlay: sftp client: %w", err)
	}
	defer client.Close()

	// Recursive sync from remote (local machine) to local (compute node)
	copied, err := syncDir(client, localWorkspace, m.MergedDir)
	if err != nil {
		return nil, fmt.Errorf("overlay: sync: %w", err)
	}

	log.Printf("[overlay] synced %d files to %s", copied, m.MergedDir)
	return m, nil
}

// syncDir recursively copies files from the SFTP remote path to a local directory.
func syncDir(client *sftp.Client, remotePath, localPath string) (int, error) {
	entries, err := client.ReadDir(remotePath)
	if err != nil {
		return 0, fmt.Errorf("readdir %s: %w", remotePath, err)
	}

	count := 0
	for _, entry := range entries {
		name := entry.Name()

		// Skip common non-essential dirs to speed up initial sync
		if entry.IsDir() && shouldSkipDir(name) {
			continue
		}

		remoteFile := filepath.Join(remotePath, name)
		localFile := filepath.Join(localPath, name)

		if entry.IsDir() {
			if err := os.MkdirAll(localFile, entry.Mode()|0700); err != nil {
				return count, fmt.Errorf("mkdir %s: %w", localFile, err)
			}
			n, err := syncDir(client, remoteFile, localFile)
			if err != nil {
				return count, err
			}
			count += n
		} else if entry.Mode().IsRegular() {
			if err := syncFile(client, remoteFile, localFile, entry.Mode()); err != nil {
				log.Printf("[overlay] warning: skip %s: %v", remoteFile, err)
				continue
			}
			count++
		}
		// Skip symlinks, devices, etc. for now
	}
	return count, nil
}

// shouldSkipDir returns true for directories that should not be synced.
func shouldSkipDir(name string) bool {
	skip := []string{
		".git", "node_modules", "__pycache__", ".venv", "venv",
		".tox", ".mypy_cache", ".pytest_cache", ".eggs",
		"target", "build", "dist", ".gradle", ".idea", ".vscode",
	}
	for _, s := range skip {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

// syncFile copies a single file from SFTP to local filesystem.
func syncFile(client *sftp.Client, remotePath, localPath string, mode os.FileMode) error {
	// Skip if local file exists and has same size (quick check)
	if info, err := os.Stat(localPath); err == nil {
		remoteInfo, err := client.Stat(remotePath)
		if err == nil && info.Size() == remoteInfo.Size() && !info.ModTime().Before(remoteInfo.ModTime()) {
			return nil // already up to date
		}
	}

	src, err := client.Open(remotePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}

// Teardown removes the synced directory.
func (m *OverlayMount) Teardown() {
	log.Printf("[overlay] cleaning up %s", m.MergedDir)
	// Don't remove MergedDir — user may have modified files that need to persist
}

