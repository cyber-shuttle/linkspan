// Package jupyter gives you a Jupyter Server. Post a root directory to /api/v1/jupyter/sessions, or name one in a
// workflow step. Linkspan will start a jupyter server there, publish its port on the tunnel, and answers with the
// URL and token that open it in a browser. Specify a port and token of your own, or let Linkspan choose one.
//
//	kind
//	newToken
//	setupMu                             Two uv runs on one environment race.
//	setup                               Installs uv without touching shell profiles, then creates the environment
//	                                    and installs the packages, idempotently, with Linkspan's stdio and uv's
//	                                    paths under install.Dir. A workflow runs it on start to build ahead of the
//	                                    first session.
//	selectSessions
//	startSession                        Spawns a server for params.root_dir on params.addr, loopback at any port by
//	                                    default: sets the environment up, publishes the port anonymously, the
//	                                    token being the credential, and runs the server with params.token, else the
//	                                    JUPYTER_TOKEN Linkspan inherited, else one it mints; an empty root_dir is
//	                                    Linkspan's own directory.
//	stopSession
//	Router                              Patterns and shapes are frozen by docs/COMPATIBILITY.md.
//	Commands                            setup, and the create and stop routes, for workflow steps.
package jupyter

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/cyber-shuttle/linkspan/internal/install"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
)

const kind tasks.Kind = "jupyter"

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var setupMu sync.Mutex

func setup(ctx context.Context, _ map[string]any) (int, any, string) {
	setupMu.Lock()
	defer setupMu.Unlock()
	root := install.Dir()
	uv, env := filepath.Join(root, "bin", "uv"), filepath.Join(root, "jupyter-env")
	steps := []struct {
		name string
		argv []string
	}{
		{"install uv", []string{"sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh"}},
		{"create environment", []string{uv, "venv", "--quiet", "--allow-existing", "--python", "3.12", env}},
		{"install packages", []string{uv, "pip", "install", "--quiet", "--python", filepath.Join(env, "bin", "python"), "jupyter-server", "ipykernel", "jupyter-server-terminals"}},
	}
	if _, err := os.Stat(uv); err == nil {
		steps = steps[1:]
	}
	for _, step := range steps {
		cmd := exec.Command(step.argv[0], step.argv[1:]...)
		cmd.Env = append(os.Environ(),
			"UV_UNMANAGED_INSTALL="+filepath.Join(root, "bin"),
			"UV_CACHE_DIR="+filepath.Join(root, "cache", "uv"),
			"UV_PYTHON_INSTALL_DIR="+filepath.Join(root, "python"))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := tasks.Exec(ctx, cmd); err != nil {
			return http.StatusInternalServerError, nil, fmt.Sprintf("%s: %v", step.name, err)
		}
	}
	return http.StatusOK, nil, ""
}

func selectSessions(context.Context, map[string]any) (int, any, string) {
	return http.StatusOK, tasks.Select(kind), ""
}

func startSession(_ context.Context, params map[string]any) (int, any, string) {
	rootDir, _ := params["root_dir"].(string)
	addr, _ := params["addr"].(string)
	token, _ := params["token"].(string)
	token = cmp.Or(token, os.Getenv("JUPYTER_TOKEN"), newToken())
	created, err := (&tasks.Task{Kind: kind, Addr: addr, Attrs: func(t tasks.Task) map[string]string {
		return map[string]string{"root_dir": rootDir, "token": token, "url": tunnel.URL(t.Port())}
	}, Spawn: func(ctx context.Context, port int) (*exec.Cmd, error) {
		if status, _, msg := setup(ctx, nil); status != http.StatusOK {
			return nil, errors.New(msg)
		}
		cmd := exec.Command(filepath.Join(install.Dir(), "jupyter-env", "bin", "python"), "-m", "jupyter_server",
			"--no-browser", "--ip=127.0.0.1", "--port="+strconv.Itoa(port), "--port-retries=0", "--ServerApp.allow_origin=*")
		cmd.Dir = rootDir
		cmd.Env = append(os.Environ(), "JUPYTER_TOKEN="+token)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd, tunnel.Publish(ctx, port, true)
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
	Prefix: "/jupyter/sessions",
	Select: selectSessions,
	Create: startSession,
	Stop:   stopSession,
})

var Commands = map[string]router.Command{"setup": setup, "sessions.start": startSession, "sessions.stop": stopSession}
