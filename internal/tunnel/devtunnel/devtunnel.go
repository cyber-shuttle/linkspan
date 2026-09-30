// Package devtunnel hosts the Dev Tunnel whoever started Linkspan made, by running the devtunnel CLI's host process as
// a task. They declare the control port on the Dev Tunnel, so the host process carries only the API and every server
// is reached through /api/v1/forward. A host process that dies returns its output, and the tunnel package reruns it.
// The host token comes from LINKSPAN_TUNNEL_HOST_TOKEN so it never shows on Linkspan's command line.
//
//	output  The last 64KB of the host process's stdout and stderr.
//	Tunnel
//	assets, cliBase
//	Write, String
//	Host    The task main starts: fetches the CLI on first use, then runs the host process until it exits or is
//	        cancelled, and returns with the captured output.
//	New     Parses the args value, so main refuses them before binding.
package devtunnel

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
)

const (
	Env   = "LINKSPAN_TUNNEL_HOST_TOKEN"
	Usage = "devtunnel transport `args`: \"--id <Dev Tunnel id> --cluster <Dev Tunnels region>\"; host token in " + Env
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

func (t *Tunnel) Host(ctx context.Context) error {
	asset, err := install.Asset(assets, "devtunnel")
	if err != nil {
		return err
	}
	bin := install.Bin("devtunnel")
	if err := install.Fetch(ctx, bin, cliBase+asset+"-devtunnel"); err != nil {
		return err
	}
	qualifiedID := t.id + "." + t.cluster
	log.Printf("devtunnel: running %s host %s --access-token [redacted]", bin, qualifiedID)
	out := &output{}
	cmd := exec.Command(bin, "host", qualifiedID, "--access-token", t.token)
	cmd.Stdout, cmd.Stderr = out, out
	err = tasks.Exec(ctx, cmd)
	if err == nil {
		err = errors.New("exit status 0")
	}
	return fmt.Errorf("host process exited (output=%q): %w", out, err)
}

func New(args, token string) (func(context.Context) error, error) {
	fs := flag.NewFlagSet("devtunnel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id, cluster := fs.String("id", "", ""), fs.String("cluster", "", "")
	if err := fs.Parse(strings.Fields(args)); err != nil || fs.NArg() > 0 || *id == "" || *cluster == "" {
		return nil, errors.New("--tunnel-devtunnel-args needs only --id and --cluster")
	}
	return (&Tunnel{id: *id, cluster: *cluster, token: token}).Host, nil
}
