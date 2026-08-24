// Package vscode is the REST surface cs-bridge drives for VS Code Remote-SSH:
// it validates a caller's public key, picks a loopback port, and hands both to
// subsystems/sshd. The SSH server itself lives there.
package vscode

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	"github.com/cyber-shuttle/linkspan/utils"
	gossh "golang.org/x/crypto/ssh"
)

type SessionRequest struct {
	AuthorizedKey string `json:"authorized_key"` // the private half never leaves the caller
}

type SessionResponse struct {
	ID       string `json:"id"`
	BindPort int32  `json:"bind_port"`
}

func ListSessions(w http.ResponseWriter, r *http.Request) {
	utils.RespondJSON(w, http.StatusOK, sshd.Statuses())
}

func CreateSession(w http.ResponseWriter, r *http.Request) {
	var req SessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	_ = r.Body.Close()

	if _, _, _, _, err := gossh.ParseAuthorizedKey([]byte(req.AuthorizedKey)); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "authorized_key is missing or invalid")
		return
	}

	port, err := utils.AvailablePort()
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The port is the identity: it is unique while the server is up, and it is
	// what the client reads back out of the id. Loopback only -- the port
	// reaches clients through the tunnel, never the node's network.
	id := fmt.Sprintf("s-%d", port)
	sshd.Start(id, fmt.Sprintf("127.0.0.1:%d", port), req.AuthorizedKey)

	utils.RespondJSON(w, http.StatusCreated, SessionResponse{ID: id, BindPort: int32(port)})
}
