// Package terminal is the capability a browser drives: a login shell served as a web terminal by ttyd, fetched on
// first use, one per session, published on the tunnel and reachable at its URL after the tunnel owner's sign-in.
// Sessions are served over /api/v1/terminal/sessions.
//
//	kind
//	ttydVersion, ttydBase, assets  The release Linkspan fetches, by platform.
//	selectSessions
//	startSession                   Answers 501 on a platform without a ttyd build; else spawns a terminal for
//	                               params.cwd: fetches ttyd and publishes the port, then runs ttyd writable on
//	                               loopback with the user's shell, or sh, as a login shell; an empty cwd is
//	                               Linkspan's own directory.
//	stopSession
//	Router                         Patterns and shapes are frozen by docs/COMPATIBILITY.md.
//	Commands                       The create and stop routes, for workflow steps.
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

func selectSessions(context.Context, map[string]any) (int, any, string) {
	return http.StatusOK, tasks.Select(kind), ""
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

func stopSession(_ context.Context, params map[string]any) (int, any, string) {
	id, _ := params["id"].(string)
	if !tasks.Stop(id) {
		return http.StatusNotFound, nil, "unknown id " + id
	}
	return http.StatusOK, map[string]string{"id": id, "state": "stopped"}, ""
}

var Router = router.New(router.Router{
	Prefix: "/terminal/sessions",
	Select: selectSessions,
	Create: startSession,
	Stop:   stopSession,
})

var Commands = map[string]router.Command{"sessions.start": startSession, "sessions.stop": stopSession}
