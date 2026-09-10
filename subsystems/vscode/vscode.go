// Package vscode gives VS Code a way in. Post your public key to /api/v1/vscode/sessions and an SSH server for
// that key alone comes up on a loopback port, listed with its id and address for as long as the job runs; point
// Remote-SSH at it through the tunnel. The server runs commands as the job's user, forwards ports and serves SFTP,
// and refuses PTYs and passwords. This package owns the wire shape; the server is internal/sshd.
//
//	kind            The SSH kind, so ids are s-<port>.
//	selectSessions
//	startSession    Serves one sshd server for params.authorized_key under tasks; the port accepts before it
//	                answers. A key carrying authorized_keys options is refused, since the server would ignore them.
//	Router          Patterns and shapes are frozen by docs/COMPATIBILITY.md.
//	Commands        sessions.start, the create route, for workflow steps.
package vscode

import (
	"context"
	"log"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sshd"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	gossh "golang.org/x/crypto/ssh"
)

const kind tasks.Kind = "sshd"

func selectSessions(context.Context, map[string]any) (int, any, string) {
	return http.StatusOK, tasks.Select(kind), ""
}

func startSession(_ context.Context, params map[string]any) (int, any, string) {
	authorizedKey, _ := params["authorized_key"].(string)
	key, _, options, _, err := gossh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return http.StatusBadRequest, nil, "authorized_key is missing or invalid"
	}
	if len(options) > 0 {
		return http.StatusBadRequest, nil, "authorized_key options are not supported"
	}
	created, err := (&tasks.Task{Kind: kind, Server: sshd.New(key)}).Start()
	if err != nil {
		return http.StatusInternalServerError, nil, err.Error()
	}
	log.Printf("vscode: listening on %s", created.Addr)
	return http.StatusCreated, map[string]any{"id": created.ID, "bind_port": created.Port()}, ""
}

var Router = router.New(router.Router{
	Prefix: "/vscode/sessions",
	Select: selectSessions,
	Create: startSession,
})

var Commands = map[string]router.Command{"sessions.start": startSession}
