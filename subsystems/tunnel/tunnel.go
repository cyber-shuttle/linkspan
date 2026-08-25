package tunnel

import (
	"bytes"
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

// linkspan hosts exactly one tunnel; main stops it on the way out.
var (
	relayMu sync.Mutex
	relay   *process
)

// StopRelay kills the hosted relay, if any. Without it the devtunnel child
// outlives linkspan -- a child is not killed when its parent exits.
func StopRelay() {
	relayMu.Lock()
	r := relay
	relay = nil
	relayMu.Unlock()
	r.kill()
}

// process is the running devtunnel CLI. The ready-wait reads output while the
// child is still writing it, hence the lock; nothing reads it afterwards and the
// relay outlives the wait inside a memory-capped cgroup, hence the cap.
type process struct {
	cmd  *exec.Cmd
	done chan struct{}

	mu     sync.Mutex
	output bytes.Buffer
}

const outputLimit = 64 << 10

func (p *process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room := outputLimit - p.output.Len(); room > 0 {
		p.output.Write(b[:min(room, len(b))])
	}
	return len(b), nil // never fail the child's write
}

func (p *process) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) kill() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
}

// stdout and stderr are collected together and never told apart.
func start(cmd *exec.Cmd) (*process, error) {
	p := &process{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p, p
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { defer close(p.done); _ = cmd.Wait() }()
	return p, nil
}

func devtunnelBin(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("devtunnel cli: resolve home dir: %w", err)
	}
	path := filepath.Join(home, ".linkspan", "bin", "devtunnel")

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
	return path, nil
}

// Via a temp file, so an interrupted transfer leaves no partial binary behind.
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
	// Executable before the rename publishes it: a crash in between would leave a
	// devtunnel that every later run finds and cannot run.
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
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

		p, err := hostOnce(ctx, tunnelID, clusterID, hostToken)
		if err == nil {
			relayMu.Lock()
			relay = p
			relayMu.Unlock()
			log.Printf("devtunnel: successfully hosting %s", tunnelID)
			return nil
		}
		p.kill()
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

// The host token authorizes hosting and nothing else, so no ports are passed.
// Every return after the relay starts carries it, so the caller can kill it.
func hostOnce(ctx context.Context, tunnelID, clusterID, hostToken string) (*process, error) {
	qualified := tunnelID
	if clusterID != "" {
		qualified = tunnelID + "." + clusterID
	}
	bin, err := devtunnelBin(ctx)
	if err != nil {
		return nil, fmt.Errorf("devtunnel host %q: %w", qualified, err)
	}

	log.Printf("devtunnel host: running %s host %s --access-token [redacted]", bin, qualified)
	//nolint:gosec // the binary path is one we downloaded to a path we chose
	p, err := start(exec.Command(bin, "host", qualified, "--access-token", hostToken))
	if err != nil {
		return nil, fmt.Errorf("devtunnel host %q: start: %w", qualified, err)
	}

	for deadline := time.Now().Add(hostReadyTimeout); time.Now().Before(deadline); {
		select {
		case <-time.After(hostReadyPoll):
		case <-ctx.Done():
			return p, ctx.Err()
		}
		if out := p.String(); strings.Contains(out, hostReadyMarker) {
			log.Printf("devtunnel host: tunnel %q ready at https://%s.devtunnels.ms", qualified, qualified)
			return p, nil
		}
		// Exiting without the marker is the failure signal; stderr is not, since
		// the CLI writes lines there and keeps hosting.
		if p.exited() {
			return p, fmt.Errorf("devtunnel host %q: exited before signalling ready (output=%q)", qualified, p.String())
		}
	}
	return p, fmt.Errorf("devtunnel host %q: no ready signal within %s (output=%q)", qualified, hostReadyTimeout, p.String())
}
