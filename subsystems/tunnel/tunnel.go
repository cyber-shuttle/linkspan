package tunnel

import (
	"context"
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
	downloadTimeout  = 5 * time.Minute
	retries          = 3
	retryDelay       = 2 * time.Second
	hostReadyMarker  = "Ready to accept connections"
	hostReadyTimeout = 30 * time.Second
	hostReadyPoll    = 500 * time.Millisecond
)

var downloadMu sync.Mutex

func devtunnelBin(ctx context.Context) (string, error) {
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
	if err := download(ctx, path, url); err != nil {
		return "", fmt.Errorf("devtunnel cli: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("devtunnel cli: chmod: %w", err)
	}
	return path, nil
}

// Via a temp file, so an interrupted transfer never leaves a partial binary
// where the next run would execute it. Bounded and cancellable: this is the one
// place linkspan blocks on the network.
func download(ctx context.Context, dst, src string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil) //nolint:gosec // src is built from a static map
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
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

// Host retries the relay bring-up, killing the relay a failed attempt left
// running. Returns nil once hosting, or when ctx ends.
func Host(ctx context.Context, tunnelID, clusterID, hostToken string) error {
	for attempt := 1; attempt <= retries; attempt++ {
		log.Printf("devtunnel: attempt %d/%d to host tunnel %s", attempt, retries, tunnelID)

		id, err := hostOnce(ctx, tunnelID, clusterID, hostToken)
		if err == nil {
			log.Printf("devtunnel: successfully hosting %s", tunnelID)
			return nil
		}
		if id != "" {
			_ = pm.Global.Kill(id)
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("devtunnel: attempt %d failed: %v", attempt, err)

		if attempt < retries {
			select {
			case <-time.After(retryDelay):
			case <-ctx.Done():
				return nil
			}
		}
	}
	return fmt.Errorf("failed to host tunnel %s after %d attempts", tunnelID, retries)
}

// The host token authorizes hosting and nothing else: linkspan never creates,
// forwards or deletes a tunnel, so no ports are passed -- the relay forwards
// whatever the client already registered.
//
// Every return after the relay starts carries its id, so the caller can kill it.
func hostOnce(ctx context.Context, tunnelID, clusterID, hostToken string) (string, error) {
	qualified := tunnelID
	if clusterID != "" {
		qualified = tunnelID + "." + clusterID
	}
	bin, err := devtunnelBin(ctx)
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
		select {
		case <-time.After(hostReadyPoll):
		case <-ctx.Done():
			return id, ctx.Err()
		}
		stdout, stderr, _ := pm.Global.Output(id)
		switch {
		case strings.Contains(stdout, hostReadyMarker):
			log.Printf("devtunnel host: tunnel %q ready at https://%s.devtunnels.ms", qualified, qualified)
			return id, nil
		// The CLI warns about things that do not stop it hosting; anything else
		// means it gave up, and waiting out the deadline adds nothing.
		case stderr != "" && !strings.Contains(stderr, "Warning"):
			return id, fmt.Errorf("devtunnel host %q: %s", qualified, stderr)
		}
	}
	stdout, stderr, _ := pm.Global.Output(id)
	return id, fmt.Errorf("devtunnel host %q: no ready signal within %s (stdout=%q stderr=%q)",
		qualified, hostReadyTimeout, stdout, stderr)
}
