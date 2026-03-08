package mount

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

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

// Static binary download URLs keyed by GOOS/GOARCH.
var fuseToolURLs = map[string]struct{ sshfs, overlayfs string }{
	"linux/amd64": {
		sshfs:     "https://github.com/cyber-shuttle/linkspan/releases/download/fuse-tools-v1/sshfs-linux-amd64",
		overlayfs: "https://github.com/cyber-shuttle/linkspan/releases/download/fuse-tools-v1/fuse-overlayfs-linux-amd64",
	},
	"linux/arm64": {
		sshfs:     "https://github.com/cyber-shuttle/linkspan/releases/download/fuse-tools-v1/sshfs-linux-arm64",
		overlayfs: "https://github.com/cyber-shuttle/linkspan/releases/download/fuse-tools-v1/fuse-overlayfs-linux-arm64",
	},
}

var fuseToolsMu sync.Mutex

// ensureFuseTool downloads a FUSE tool binary to ~/.linkspan/bin/ if not
// already present. Returns the absolute path to the binary.
func ensureFuseTool(name, url string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}

	binDir := filepath.Join(home, ".linkspan", "bin")
	binPath := filepath.Join(binDir, name)

	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	fuseToolsMu.Lock()
	defer fuseToolsMu.Unlock()

	// Re-check after acquiring lock
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	log.Printf("[overlay] downloading %s from %s", name, url)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create bin dir: %w", err)
	}

	//nolint:gosec,noctx
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %s", name, resp.Status)
	}

	tmp, err := os.CreateTemp(binDir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		if _, e := os.Stat(tmpPath); e == nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}

	log.Printf("[overlay] %s ready at %s", name, binPath)
	return binPath, nil
}

// resolveBin returns the path to a binary, checking system PATH first and
// falling back to a managed download.
func resolveBin(name string, downloadURL string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if downloadURL == "" {
		return "", fmt.Errorf("%s not found in PATH and no download URL for %s/%s", name, runtime.GOOS, runtime.GOARCH)
	}
	return ensureFuseTool(name, downloadURL)
}

// SetupOverlay creates the overlay filesystem:
// 1. sshfs mounts the local workspace via tunnel
// 2. fuse-overlayfs merges lower (sshfs) + upper (cache)
func SetupOverlay(sessionID string, localSshPort int, localWorkspace string) (*OverlayMount, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("overlay: get home dir: %w", err)
	}

	key := runtime.GOOS + "/" + runtime.GOARCH
	urls := fuseToolURLs[key] // zero value if not found

	sshfsBin, err := resolveBin("sshfs", urls.sshfs)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}

	overlayBin, err := resolveBin("fuse-overlayfs", urls.overlayfs)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
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
		"-f", // foreground — required for process manager tracking
		"-p", fmt.Sprintf("%d", localSshPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}

	sshfsCmd := exec.Command(sshfsBin, sshfsArgs...)
	sshfsCmdID, err := pm.GlobalProcessManager.Start(sshfsCmd)
	if err != nil {
		return nil, fmt.Errorf("overlay: start sshfs: %w", err)
	}
	m.SshfsCmdID = sshfsCmdID

	// Brief pause to let sshfs establish the mount
	time.Sleep(2 * time.Second)
	log.Printf("[overlay] sshfs mounted %s on port %d → %s", localWorkspace, localSshPort, m.SourceDir)

	// 2. fuse-overlayfs
	overlayArgs := []string{
		"-o", fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", m.SourceDir, m.CacheDir, m.WorkDir),
		m.MergedDir,
	}

	overlayCmd := exec.Command(overlayBin, overlayArgs...)
	overlayCmdID, err := pm.GlobalProcessManager.Start(overlayCmd)
	if err != nil {
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
	_ = exec.Command("fusermount", "-u", m.MergedDir).Run()
	_ = exec.Command("fusermount", "-u", m.SourceDir).Run()

	if m.SshfsCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(m.SshfsCmdID)
		log.Printf("[overlay] stopped sshfs for %s", m.SessionID)
	}
}
