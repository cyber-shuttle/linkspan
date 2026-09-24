// Package vscode serves VS Code Remote-SSH. A public key posted to /api/v1/vscode/sessions starts an SSH server
// on a loopback port that accepts that key alone, listed with its id and address while the job runs. The server
// runs commands as the job's user, forwards ports and serves SFTP; it refuses PTYs and passwords. This package
// owns the wire shape, and the server is internal/sshd.
//
//	kind            The SSH kind, so ids are s-<port>.
//	startSession    Serves one sshd server for params.authorized_key under tasks; the port accepts before it
//	                answers. A key carrying authorized_keys options is refused, since the server would ignore them. A
//	                ref already serving is answered 200 as it is, so a client that names its key reuses one server.
//	Commands        sessions.select, from sessions, and sessions.start; shapes are frozen by docs/COMPATIBILITY.md.
//	Router          /vscode/sessions, each route a Commands entry.
package vscode

import (
	"context"
	"log"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/sessions"
	"github.com/cyber-shuttle/linkspan/internal/sshd"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
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
	ref := sessions.Ref(params)
	for _, running := range tasks.Select(kind) {
		if running.ID == ref && running.State == tasks.StateRunning {
			return http.StatusOK, map[string]any{"id": running.ID, "bind_port": running.Port()}, ""
		}
	}
	created, err := (&tasks.Task{ID: ref, Kind: kind, Server: sshd.New(key)}).Start()
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
