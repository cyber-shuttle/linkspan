// Package terminal serves a login shell in the browser. A working directory posted to /api/v1/terminal/sessions
// starts ttyd, fetched on first use, publishes its port on the tunnel and answers with the URL, which opens after
// the tunnel owner signs in. Linux only, one shell per session.
//
//	kind
//	ttydVersion, ttydBase, assets  The release Linkspan fetches, by platform.
//	startSession                   Answers 501 on a platform without a ttyd build; else spawns a terminal for
//	                               params.cwd: fetches ttyd, publishes the port and runs ttyd writable on loopback
//	                               with the user's shell, or sh, as a login shell; an empty cwd is Linkspan's own
//	                               directory.
//	Commands                       sessions.start, with sessions.select and sessions.stop from sessions; shapes
//	                               are frozen by docs/COMPATIBILITY.md.
//	Router                         /terminal/sessions, each route a Commands entry.
package terminal

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
)

const kind tasks.Kind = "terminal"

const (
	ttydVersion = "1.7.7"
	ttydBase    = "https://github.com/tsl0922/ttyd/releases/download/"
)

var assets = map[string]string{
	"linux/amd64": "ttyd.x86_64",
	"linux/arm64": "ttyd.aarch64",
}

func startSession(_ context.Context, params map[string]any) (int, any, string) {
	asset, ok := assets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return http.StatusNotImplemented, nil, fmt.Sprintf("no ttyd binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	cwd, _ := params["cwd"].(string)
	created, err := (&tasks.Task{Kind: kind, Attrs: func(t tasks.Task) map[string]string {
		return map[string]string{"cwd": cwd, "url": tunnel.URL(t.Port())}
	}, Spawn: func(ctx context.Context, port int) (*exec.Cmd, error) {
		bin := filepath.Join(install.Dir(), "bin", "ttyd")
		if err := install.Fetch(ctx, bin, ttydBase+ttydVersion+"/"+asset); err != nil {
			return nil, err
		}
		cmd := exec.Command(bin, "-p", strconv.Itoa(port), "-i", "127.0.0.1", "-W", cmp.Or(os.Getenv("SHELL"), "sh"), "-l")
		cmd.Dir = cwd
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, tunnel.Publish(ctx, port, false)
	}}).Start()
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusCreated, created, ""
}

var Commands = map[string]router.Command{
	"sessions.select": sessions.Select(kind),
	"sessions.start":  startSession,
	"sessions.stop":   sessions.Stop,
}

var Router = router.New("/terminal", map[string]router.Command{
	"GET /sessions":         Commands["sessions.select"],
	"POST /sessions":        Commands["sessions.start"],
	"DELETE /sessions/{id}": Commands["sessions.stop"],
})
