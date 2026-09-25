// Package devtunnel hosts the Dev Tunnel a client created, by running the devtunnel CLI, the relay, as a task. The
// client declares the control port on the tunnel, so the relay carries only the API and every server is reached
// through /api/v1/forward. A relay that dies returns its output, and the tunnel package reruns it. The host token is read from
// LINKSPAN_TUNNEL_HOST_TOKEN so it never shows on Linkspan's command line.
//
//	output  The last 64KB of the relay's stdout and stderr.
//	Tunnel
//	assets, cliBase
//	Write, String
//	Relay   The task main starts: fetches the CLI on first use, then runs the relay until it exits or is
//	        cancelled, and returns with the captured output.
//	New     Parses the args value and validates it with the token, so main refuses them before binding.
package devtunnel

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const (
	Env   = "LINKSPAN_TUNNEL_HOST_TOKEN"
	Usage = "devtunnel mode `args`: \"--id <tunnel id> --cluster <cluster id>\"; host token in " + Env
)

type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type Tunnel struct {
	id, cluster, token string
}

var assets = map[string]string{
	"linux/amd64":  "linux-x64",
	"linux/arm64":  "linux-arm64",
	"darwin/amd64": "osx-x64",
	"darwin/arm64": "osx-arm64",
}

var cliBase = "https://tunnelsassetsprod.blob.core.windows.net/cli/"

func (o *output) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(b)
	if over := o.buf.Len() - 64<<10; over > 0 {
		o.buf.Next(over)
	}
	return len(b), nil
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (t *Tunnel) Relay(ctx context.Context) error {
	asset, err := install.Asset(assets, "devtunnel")
	if err != nil {
		return err
	}
	bin := filepath.Join(install.Dir(), "bin", "devtunnel")
	if err := install.Fetch(ctx, bin, cliBase+asset+"-devtunnel"); err != nil {
		return err
	}
	qualifiedID := t.id + "." + t.cluster
	log.Printf("tunnel: running %s host %s --access-token [redacted]", bin, qualifiedID)
	out := &output{}
	cmd := exec.Command(bin, "host", qualifiedID, "--access-token", t.token)
	cmd.Stdout, cmd.Stderr = out, out
	err = tasks.Exec(ctx, cmd)
	if err == nil {
		err = errors.New("exit status 0")
	}
	return fmt.Errorf("relay exited (output=%q): %w", out, err)
}

func New(args string) (func(context.Context) error, error) {
	fs := flag.NewFlagSet("devtunnel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id, cluster, token := fs.String("id", "", ""), fs.String("cluster", "", ""), os.Getenv(Env)
	if err := fs.Parse(strings.Fields(args)); err != nil || fs.NArg() > 0 || *id == "" || *cluster == "" {
		return nil, errors.New("--tunnel-devtunnel-args needs only --id and --cluster")
	}
	if token == "" {
		return nil, errors.New(Env + " is required with the devtunnel mode")
	}
	return (&Tunnel{id: *id, cluster: *cluster, token: token}).Relay, nil
}
