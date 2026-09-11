// Package tunnel hosts the Dev Tunnel a client created, by running the devtunnel CLI, the relay, as a task, and
// publishes ports on it for the servers the subsystems start. A relay that dies ends the task and is not
// restarted.
//
//	apiVersion     The Dev Tunnels REST API version portRequest sends.
//	readyLine      What the relay prints once it is hosting.
//	output         The last 64KB of the relay's stdout and stderr; onReady runs once the ready line has appeared.
//	Tunnel
//	assets, cliBase, apiBase, active, none  cliBase and apiBase are test seams; none is closed, for no tunnel.
//	Write, String
//	portRequest    One ports request for the active tunnel with the host token; any status outside 2xx fails, and the
//	               token is redacted from any error.
//	Relay          The task main starts: fetches the CLI on first use, then runs the relay until it exits or is
//	               cancelled, and returns with the captured output.
//	New            Validates its inputs, so main refuses them before binding, and makes it the active tunnel.
//	Ready          Closed once the active tunnel's relay is hosting, or already closed when no tunnel is hosted.
//	URL            A port's public address on the active tunnel, or "" when no tunnel is hosted.
//	Publish        Adds a port to the active tunnel for as long as ctx lives and removes it after, anonymous when the
//	               server behind it has a credential of its own; the hosting relay picks both up on its own. No
//	               tunnel is not an error.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const apiVersion = "2023-09-27-preview"

var readyLine = []byte("Ready to accept connections")

type output struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	onReady func()
}

type Tunnel struct {
	id, cluster, token string
	ready              chan struct{}
	readyOnce          sync.Once
}

var assets = map[string]string{
	"linux/amd64":  "linux-x64",
	"linux/arm64":  "linux-arm64",
	"darwin/amd64": "osx-x64",
	"darwin/arm64": "osx-arm64",
}

var (
	cliBase = "https://tunnelsassetsprod.blob.core.windows.net/cli/"
	apiBase = "https://{cluster}.rel.tunnels.api.visualstudio.com"
)

var active atomic.Pointer[Tunnel]

var none = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (o *output) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(b)
	if over := o.buf.Len() - 64<<10; over > 0 {
		o.buf.Next(over)
	}
	if o.onReady != nil && bytes.Contains(o.buf.Bytes(), readyLine) {
		o.onReady()
		o.onReady = nil
	}
	return len(b), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (t *Tunnel) portRequest(ctx context.Context, method string, port int, anonymous bool) error {
	spec := map[string]any{"portNumber": port, "protocol": "http"}
	if anonymous {
		spec["accessControl"] = map[string]any{"entries": []map[string]any{{"type": "Anonymous", "subjects": []string{}, "scopes": []string{"connect"}}}}
	}
	body, _ := json.Marshal(spec)
	url := strings.Replace(apiBase, "{cluster}", t.cluster, 1) + "/tunnels/" + t.id + "/ports/" + strconv.Itoa(port) + "?api-version=" + apiVersion
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "tunnel "+t.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), t.token, "[redacted]"))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s port %d: HTTP %d: %s", method, port, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *Tunnel) Relay(ctx context.Context) error {
	asset, ok := assets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no devtunnel binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	bin := filepath.Join(install.Dir(), "bin", "devtunnel")
	if err := install.Fetch(ctx, bin, cliBase+asset+"-devtunnel"); err != nil {
		return err
	}
	qualifiedID := t.id + "." + t.cluster
	log.Printf("tunnel: running %s host %s --access-token [redacted]", bin, qualifiedID)
	out := &output{onReady: func() { t.readyOnce.Do(func() { close(t.ready) }) }}
	cmd := exec.Command(bin, "host", qualifiedID, "--access-token", t.token)
	cmd.Stdout, cmd.Stderr = out, out
	err := tasks.Exec(ctx, cmd)
	if err == nil {
		err = errors.New("exit status 0")
	}
	return fmt.Errorf("relay exited (output=%q): %w", out, err)
}

func New(id, cluster, token string) (*Tunnel, error) {
	if id == "" || cluster == "" || token == "" {
		return nil, errors.New("tunnel: id, cluster and host token are all required")
	}
	t := &Tunnel{id: id, cluster: cluster, token: token, ready: make(chan struct{})}
	active.Store(t)
	return t, nil
}

func Ready() <-chan struct{} {
	if t := active.Load(); t != nil {
		return t.ready
	}
	return none
}

func URL(port int) string {
	t := active.Load()
	if t == nil {
		return ""
	}
	return fmt.Sprintf("https://%s-%d.%s.devtunnels.ms", t.id, port, t.cluster)
}

func Publish(ctx context.Context, port int, anonymous bool) error {
	t := active.Load()
	if t == nil {
		return nil
	}
	if err := t.portRequest(ctx, http.MethodPut, port, anonymous); err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	context.AfterFunc(ctx, func() { _ = t.portRequest(context.Background(), http.MethodDelete, port, false) })
	return nil
}
