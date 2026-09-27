// Package terminal serves a PTY in the browser. A working directory posted to /api/v1/terminal/sessions
// starts ttyd on loopback, fetched on first use, and answers with its address. Linux only, one PTY per server.
//
//	kind
//	ttydVersion, ttydBase, assets  The release Linkspan fetches, by platform.
//	startServer                    Answers 501 on a platform without a ttyd build; else spawns a terminal for
//	                               params.cwd: fetches ttyd and runs it writable on loopback running $SHELL, or
//	                               sh, with -l on the PTY; an empty cwd is Linkspan's own directory.
//	Actions                        sessions.start, with sessions.select and sessions.stop from servers; shapes
//	                               are frozen by docs/COMPATIBILITY.md.
//	Router                         /terminal/sessions, each route an Actions entry.
package terminal

import (
	"cmp"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/servers"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
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

func startServer(_ context.Context, params map[string]any) (int, any, string) {
	asset, err := install.Asset(assets, "ttyd")
	if err != nil {
		return http.StatusNotImplemented, nil, err.Error()
	}
	cwd, _ := params["cwd"].(string)
	created, err := (&tasks.Task{ID: servers.Ref(params), Kind: kind, Attrs: func(tasks.Task) map[string]string {
		return map[string]string{"cwd": cwd}
	}, Spawn: func(ctx context.Context, port int) (*exec.Cmd, error) {
		bin := filepath.Join(install.Dir(), "bin", "ttyd")
		if err := install.Fetch(ctx, bin, ttydBase+ttydVersion+"/"+asset); err != nil {
			return nil, err
		}
		cmd := exec.Command(bin, "-p", strconv.Itoa(port), "-i", "127.0.0.1", "-W", cmp.Or(os.Getenv("SHELL"), "sh"), "-l")
		cmd.Dir = cwd
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, nil
	}}).Start()
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	return http.StatusCreated, created, ""
}

var Actions = map[string]router.Action{
	"sessions.select": servers.Select(kind),
	"sessions.start":  startServer,
	"sessions.stop":   servers.Stop,
}

var Router = router.New("/terminal", map[string]router.Action{
	"GET /sessions":         Actions["sessions.select"],
	"POST /sessions":        Actions["sessions.start"],
	"DELETE /sessions/{id}": Actions["sessions.stop"],
})
