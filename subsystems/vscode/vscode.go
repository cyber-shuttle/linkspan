// Package vscode serves VS Code Remote-SSH. A public key posted to /api/v1/vscode/sessions starts an SSH server
// on a loopback port that accepts that key alone, listed with its id and address while the job runs. The server
// runs commands as the job's user, forwards ports and serves SFTP; it refuses PTYs and passwords. This package
// owns the wire shape, and the server is internal/sshd.
//
//	kind            The SSH kind, so ids are s-<port>.
//	startSession    Serves one sshd server for params.authorized_key under tasks; the port accepts before it
//	                answers. A key carrying authorized_keys options is refused, since the server would ignore them.
//	Commands        sessions.select, from sessions, and sessions.start; shapes are frozen by docs/COMPATIBILITY.md.
//	Router          /vscode/sessions, each route a Commands entry.
package vscode

import (
	"context"
	"log"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sshd"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/subsystems/sessions"
	gossh "golang.org/x/crypto/ssh"
)

const kind tasks.Kind = "sshd"

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

var Commands = map[string]router.Command{
	"sessions.select": sessions.Select(kind),
	"sessions.start":  startSession,
}

var Router = router.New("/vscode", map[string]router.Command{
	"GET /sessions":  Commands["sessions.select"],
	"POST /sessions": Commands["sessions.start"],
})
