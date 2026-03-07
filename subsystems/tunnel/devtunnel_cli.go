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
	"github.com/cyber-shuttle/linkspan/utils"
)

// devtunnelDownloadURLs maps GOOS/GOARCH pairs to the Azure blob storage URLs
// for the latest devtunnel CLI binary for each supported platform.
var devtunnelDownloadURLs = map[string]string{
	"linux/amd64":   "https://tunnelsassetsprod.blob.core.windows.net/cli/linux-x64-devtunnel",
	"linux/arm64":   "https://tunnelsassetsprod.blob.core.windows.net/cli/linux-arm64-devtunnel",
	"darwin/amd64":  "https://tunnelsassetsprod.blob.core.windows.net/cli/osx-x64-devtunnel",
	"darwin/arm64":  "https://tunnelsassetsprod.blob.core.windows.net/cli/osx-arm64-devtunnel",
	"windows/amd64": "https://tunnelsassetsprod.blob.core.windows.net/cli/win-x64-devtunnel.exe",
}

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
	if runtime.GOOS == "windows" {
		binName = "devtunnel.exe"
	}

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

// cliCommand builds an *exec.Cmd for the given binary and arguments.
func cliCommand(binary string, args ...string) *exec.Cmd {
	return exec.Command(binary, args...) //nolint:gosec // binary path is controlled by us
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

// CLIHostTunnel starts hosting a tunnel using the managed devtunnel binary and a
// host-scoped access token obtained from the SDK (no interactive CLI login needed).
//
// It returns:
//   - commandID — the ProcessManager ID of the background `devtunnel host` process,
//     so the caller can kill it later via pm.GlobalProcessManager.Kill.
//   - connectionURL — the "Connect via browser:" URL parsed from the CLI output.
func CLIHostTunnel(tunnelID string, hostToken string, ports []int) (commandID string, connectionURL string, err error) {
	binPath, err := devtunnelBinPath()
	if err != nil {
		return "", "", fmt.Errorf("devtunnel cli: get binary: %w", err)
	}

	// The devtunnel CLI accepts a tunnel-scoped host token via --access-token.
	// We pass the tunnel ID so the CLI can connect directly without a lookup.
	// -p tells the host which local ports to forward through the tunnel.
	args := []string{"host", tunnelID, "--access-token", hostToken}
	for _, p := range ports {
		args = append(args, "-p", fmt.Sprintf("%d", p))
	}
	log.Printf("devtunnel cli: running: %s %v", binPath, args)

	cmd := cliCommand(binPath, args...)
	cmdID, err := pm.GlobalProcessManager.Start(cmd)
	if err != nil {
		return "", "", fmt.Errorf("devtunnel cli: start host command: %w", err)
	}

	// Wait for the CLI to signal it is ready.
	// - With ports: look for "Connect via browser:" which includes a port-specific URL.
	// - Without ports: look for "Ready to accept connections" and build the URL ourselves.
	const pollInterval = 500 * time.Millisecond
	const maxWait = 30 * time.Second
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		stdout, _, _ := pm.GlobalProcessManager.GetOutput(cmdID)

		// With ports forwarded, the CLI prints a browser-accessible URL.
		if len(ports) > 0 {
			url, findErr := utils.FindLineInStdout(stdout, "Connect via browser:")
			if findErr == nil && url != "" {
				return cmdID, url, nil
			}
		}

		// Without ports (or as a fallback), "Ready to accept connections" means
		// the relay is up.  Build the connection URL from the tunnel ID.
		if strings.Contains(stdout, "Ready to accept connections") {
			connURL := fmt.Sprintf("https://%s.devtunnels.ms", tunnelID)
			return cmdID, connURL, nil
		}
	}

	// Timed out — collect what we have for the error message.
	stdout, stderr, _ := pm.GlobalProcessManager.GetOutput(cmdID)
	return "", "", fmt.Errorf(
		"devtunnel cli: timed out waiting for ready signal (stdout=%q stderr=%q)",
		stdout, stderr,
	)
}

// CLIConnectTunnel connects to an existing hosted tunnel, making its forwarded
// ports available on localhost. It runs `devtunnel connect` as a background
// process and waits until the connection is established.
//
// Returns the ProcessManager command ID so the caller can kill it later.
func CLIConnectTunnel(tunnelID string, accessToken string) (commandID string, err error) {
	binPath, err := devtunnelBinPath()
	if err != nil {
		return "", fmt.Errorf("devtunnel cli: get binary: %w", err)
	}

	args := []string{"connect", tunnelID, "--access-token", accessToken}
	log.Printf("devtunnel cli: running: %s %v", binPath, args)

	cmd := cliCommand(binPath, args...)
	cmdID, err := pm.GlobalProcessManager.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("devtunnel cli: start connect command: %w", err)
	}

	// Wait for the connection to be established. The CLI prints a line
	// containing "Connected" or "Forwarding port" when ready.
	const pollInterval = 500 * time.Millisecond
	const maxWait = 60 * time.Second
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		stdout, stderr, _ := pm.GlobalProcessManager.GetOutput(cmdID)
		combined := stdout + stderr
		if strings.Contains(combined, "Connected") || strings.Contains(combined, "Forwarding port") || strings.Contains(combined, "Ready to accept connections") {
			log.Printf("devtunnel cli: connect established for tunnel %s", tunnelID)
			return cmdID, nil
		}

		// Check if process already exited with an error
		info, infoErr := pm.GlobalProcessManager.GetInfo(cmdID)
		if infoErr == nil && info.Completed {
			return "", fmt.Errorf("devtunnel cli: connect exited prematurely (stdout=%q stderr=%q)", stdout, stderr)
		}
	}

	// Timed out — kill the process and report
	_ = pm.GlobalProcessManager.Kill(cmdID)
	stdout, stderr, _ := pm.GlobalProcessManager.GetOutput(cmdID)
	return "", fmt.Errorf(
		"devtunnel cli: timed out waiting for connect (stdout=%q stderr=%q)",
		stdout, stderr,
	)
}
