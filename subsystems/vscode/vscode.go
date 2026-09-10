// Package vscode is the capability cs-bridge drives: one SSH server per public key for VS Code Remote-SSH, listed and
// started over /api/v1/vscode/sessions. The servers are internal/sshd; this package owns the wire shape.
//
//	kind            The SSH kind, so ids are s-<port>.
//	selectSessions
//	startSession    Parses params.authorized_key and serves one sshd server for it under tasks; the port accepts
//	                before it answers. A key carrying authorized_keys options is refused, because the server would
//	                ignore them.
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
