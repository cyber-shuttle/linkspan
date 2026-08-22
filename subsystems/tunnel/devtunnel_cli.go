package tunnel

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
)

// devtunnelDownloadURLs maps GOOS/GOARCH pairs to the Azure blob storage URLs
// for the latest devtunnel CLI binary for each supported platform.
var devtunnelDownloadURLs = map[string]string{
	"linux/amd64":  "https://tunnelsassetsprod.blob.core.windows.net/cli/linux-x64-devtunnel",
	"linux/arm64":  "https://tunnelsassetsprod.blob.core.windows.net/cli/linux-arm64-devtunnel",
	"darwin/amd64": "https://tunnelsassetsprod.blob.core.windows.net/cli/osx-x64-devtunnel",
	"darwin/arm64": "https://tunnelsassetsprod.blob.core.windows.net/cli/osx-arm64-devtunnel",
}

const (
	hostReadyMarker  = "Ready to accept connections"
	hostReadyTimeout = 30 * time.Second
	hostReadyPoll    = 500 * time.Millisecond
)

// binaryDownloadMu prevents concurrent downloads of the same binary.
var binaryDownloadMu sync.Mutex

// devtunnelBinPath returns the absolute path to the managed devtunnel binary,
// downloading it on first use.  The binary is stored at
// ~/.linkspan/bin/devtunnel (or devtunnel.exe on Windows).
func devtunnelBinPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("devtunnel cli: resolve home dir: %w", err)
	}

	binName := "devtunnel"

	binDir := filepath.Join(home, ".linkspan", "bin")
	binPath := filepath.Join(binDir, binName)

	// Fast path: binary already present.
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	// Slow path: download under a mutex to avoid concurrent downloads.
	binaryDownloadMu.Lock()
	defer binaryDownloadMu.Unlock()

	// Re-check after acquiring the lock in case another goroutine finished first.
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	key := runtime.GOOS + "/" + runtime.GOARCH
	downloadURL, ok := devtunnelDownloadURLs[key]
	if !ok {
		return "", fmt.Errorf("devtunnel cli: no binary available for platform %s", key)
	}

	log.Printf("devtunnel cli: downloading binary from %s -> %s", downloadURL, binPath)

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("devtunnel cli: create bin dir %s: %w", binDir, err)
	}

	if err := downloadFile(binPath, downloadURL); err != nil {
		return "", fmt.Errorf("devtunnel cli: download binary: %w", err)
	}

	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", fmt.Errorf("devtunnel cli: chmod binary: %w", err)
	}

	log.Printf("devtunnel cli: binary ready at %s", binPath)
	return binPath, nil
}

// downloadFile fetches src (following redirects) and writes the response body to
// dst, replacing any existing file.
func downloadFile(dst, src string) error {
	//nolint:noctx // simple download, no cancellation required
	resp, err := http.Get(src) //nolint:gosec // URL is from a static map
	if err != nil {
		return fmt.Errorf("GET %s: %w", src, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", src, resp.Status)
	}

	f, err := os.CreateTemp(filepath.Dir(dst), ".devtunnel-download-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := f.Name()
	defer func() {
		f.Close()
		// Clean up temp file on any error path.
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write download: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Atomic rename so we never leave a partial binary at the destination.
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, dst, err)
	}
	return nil
}

// DevTunnelHost runs the relay for a tunnel the client owns and registers ports
// on. The host token authorizes hosting and nothing else: linkspan never
// creates, forwards or deletes a tunnel, so no ports are passed here -- the
// relay forwards whatever the client already registered on the service.
func DevTunnelHost(tunnelID, clusterID, hostToken string) (string, error) {
	qualified := tunnelID
	if clusterID != "" {
		qualified = tunnelID + "." + clusterID
	}
	binPath, err := devtunnelBinPath()
	if err != nil {
		return "", fmt.Errorf("devtunnel host %q: %w", qualified, err)
	}
	log.Printf("devtunnel host: running %s host %s --access-token [redacted]", binPath, qualified)
	//nolint:gosec // the binary path is one we downloaded to a path we chose
	cmdID, err := pm.GlobalProcessManager.Start(exec.Command(binPath, "host", qualified, "--access-token", hostToken))
	if err != nil {
		return "", fmt.Errorf("devtunnel host %q: start: %w", qualified, err)
	}

	deadline := time.Now().Add(hostReadyTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(hostReadyPoll)
		stdout, stderr, _ := pm.GlobalProcessManager.GetOutput(cmdID)
		if strings.Contains(stdout, hostReadyMarker) {
			log.Printf("devtunnel host: tunnel %q ready at https://%s.devtunnels.ms", qualified, qualified)
			return cmdID, nil
		}
		// The CLI warns about things that do not stop it hosting; anything else
		// on stderr means it gave up, and waiting out the deadline adds nothing.
		if stderr != "" && !strings.Contains(stderr, "Warning") {
			return "", fmt.Errorf("devtunnel host %q: %s", qualified, stderr)
		}
	}
	stdout, stderr, _ := pm.GlobalProcessManager.GetOutput(cmdID)
	return "", fmt.Errorf("devtunnel host %q: no ready signal within %s (stdout=%q stderr=%q)",
		qualified, hostReadyTimeout, stdout, stderr)
}
