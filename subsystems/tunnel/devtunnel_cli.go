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

var devtunnelAsset = map[string]string{
	"linux/amd64":  "linux-x64",
	"linux/arm64":  "linux-arm64",
	"darwin/amd64": "osx-x64",
	"darwin/arm64": "osx-arm64",
}

const (
	devtunnelURL     = "https://tunnelsassetsprod.blob.core.windows.net/cli/%s-devtunnel"
	hostReadyMarker  = "Ready to accept connections"
	hostReadyTimeout = 30 * time.Second
	hostReadyPoll    = 500 * time.Millisecond
)

var downloadMu sync.Mutex

func devtunnelBin() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("devtunnel cli: resolve home dir: %w", err)
	}
	path := filepath.Join(home, ".linkspan", "bin", "devtunnel")

	downloadMu.Lock()
	defer downloadMu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := devtunnelAsset[platform]
	if !ok {
		return "", fmt.Errorf("devtunnel cli: no binary for platform %s", platform)
	}
	url := fmt.Sprintf(devtunnelURL, asset)

	log.Printf("devtunnel cli: downloading %s -> %s", url, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("devtunnel cli: create bin dir: %w", err)
	}
	if err := download(path, url); err != nil {
		return "", fmt.Errorf("devtunnel cli: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("devtunnel cli: chmod: %w", err)
	}
	return path, nil
}

// Via a temp file, so an interrupted transfer never leaves a partial binary
// where the next run would execute it.
func download(dst, src string) error {
	//nolint:noctx // one-shot download, nothing to cancel it from
	resp, err := http.Get(src) //nolint:gosec // src is built from a static map
	if err != nil {
		return fmt.Errorf("GET %s: %w", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", src, resp.Status)
	}

	f, err := os.CreateTemp(filepath.Dir(dst), ".devtunnel-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once the rename below succeeds

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("write download: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

// The host token authorizes hosting and nothing else: linkspan never creates,
// forwards or deletes a tunnel, so no ports are passed -- the relay forwards
// whatever the client already registered.
func DevTunnelHost(tunnelID, clusterID, hostToken string) (string, error) {
	qualified := tunnelID
	if clusterID != "" {
		qualified = tunnelID + "." + clusterID
	}
	bin, err := devtunnelBin()
	if err != nil {
		return "", fmt.Errorf("devtunnel host %q: %w", qualified, err)
	}

	log.Printf("devtunnel host: running %s host %s --access-token [redacted]", bin, qualified)
	//nolint:gosec // the binary path is one we downloaded to a path we chose
	id, err := pm.Global.Start(exec.Command(bin, "host", qualified, "--access-token", hostToken))
	if err != nil {
		return "", fmt.Errorf("devtunnel host %q: start: %w", qualified, err)
	}

	for deadline := time.Now().Add(hostReadyTimeout); time.Now().Before(deadline); {
		time.Sleep(hostReadyPoll)
		stdout, stderr, _ := pm.Global.Output(id)
		switch {
		case strings.Contains(stdout, hostReadyMarker):
			log.Printf("devtunnel host: tunnel %q ready at https://%s.devtunnels.ms", qualified, qualified)
			return id, nil
		// The CLI warns about things that do not stop it hosting; anything else
		// means it gave up, and waiting out the deadline adds nothing.
		case stderr != "" && !strings.Contains(stderr, "Warning"):
			return "", fmt.Errorf("devtunnel host %q: %s", qualified, stderr)
		}
	}
	stdout, stderr, _ := pm.Global.Output(id)
	return "", fmt.Errorf("devtunnel host %q: no ready signal within %s (stdout=%q stderr=%q)",
		qualified, hostReadyTimeout, stdout, stderr)
}
