// Package tunnel hosts the Dev Tunnel the client created by running the
// devtunnel CLI, the relay, under procmgr. A relay that dies ends the task and
// is not restarted; StopAll kills it and waits.
//
//	readyMarker
//	output                   It holds the last 64KB of the relay's stdout and
//	                         stderr, and ready is closed as the ready marker is
//	                         written.
//	Tunnel                   Its host method fetches the CLI on first use, runs
//	                         it, and returns on relay exit, on hostReadyTimeout
//	                         without the ready marker, or on cancellation, always
//	                         with the captured output. Start registers it under
//	                         procmgr as "tunnel". hostReadyTimeout is a test
//	                         seam.
//	downloadDevtunnelBinary  It owns ~/.linkspan/bin/devtunnel: it returns the
//	                         path when the file is present, and otherwise fetches
//	                         this platform's CLI through a sibling file renamed
//	                         into place. cliBase is a test seam.
//	New                      It validates its inputs, so main refuses them before
//	                         binding.
package tunnel

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/cyber-shuttle/linkspan/internal/procmgr"
)

const readyMarker = "Ready to accept connections"

type output struct {
	ready chan struct{}

	mu  sync.Mutex
	buf bytes.Buffer
}

type Tunnel struct {
	qualifiedID, token string
}

var hostReadyTimeout = 30 * time.Second

var cliBase = "https://tunnelsassetsprod.blob.core.windows.net/cli/"

func downloadDevtunnelBinary(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dst := filepath.Join(home, ".linkspan", "bin", "devtunnel")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := map[string]string{
		"linux/amd64":  "linux-x64",
		"linux/arm64":  "linux-arm64",
		"darwin/amd64": "osx-x64",
		"darwin/arm64": "osx-arm64",
	}[platform]
	if !ok {
		return "", fmt.Errorf("no devtunnel binary for %s", platform)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	src := cliBase + asset + "-devtunnel"
	log.Printf("tunnel: downloading %s -> %s", src, dst)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected status %s", src, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", src, err)
	}
	part := dst + ".part"
	defer func() { _ = os.Remove(part) }()
	if err := os.WriteFile(part, data, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (o *output) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(b)
	if over := o.buf.Len() - 64<<10; over > 0 {
		o.buf.Next(over)
	}
	select {
	case <-o.ready:
	default:
		if bytes.Contains(o.buf.Bytes(), []byte(readyMarker)) {
			close(o.ready)
		}
	}
	return len(b), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func New(id, cluster, token string) (*Tunnel, error) {
	if id == "" || cluster == "" || token == "" {
		return nil, errors.New("tunnel: id, cluster and host token are all required")
	}
	return &Tunnel{qualifiedID: id + "." + cluster, token: token}, nil
}

func (t *Tunnel) host(ctx context.Context) error {
	bin, err := downloadDevtunnelBinary(ctx)
	if err != nil {
		return err
	}
	log.Printf("tunnel: running %s host %s --access-token [redacted]", bin, t.qualifiedID)
	out := &output{ready: make(chan struct{})}
	cmd := exec.Command(bin, "host", t.qualifiedID, "--access-token", t.token)
	cmd.Stdout, cmd.Stderr = out, out
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exited := make(chan error, 1)
	go func() { exited <- procmgr.Exec(ctx, cmd) }()
	select {
	case <-out.ready:
		log.Printf("tunnel: %s ready", t.qualifiedID)
		err = <-exited
	case <-time.After(hostReadyTimeout):
		cancel()
		<-exited
		return fmt.Errorf("relay killed: no ready signal within %s (output=%q)", hostReadyTimeout, out)
	case err = <-exited:
	}
	if err == nil {
		err = errors.New("exit status 0")
	}
	return fmt.Errorf("relay exited (output=%q): %w", out, err)
}

func (t *Tunnel) Start() {
	procmgr.Start(procmgr.KindTunnel, "tunnel", "", t.host)
}
