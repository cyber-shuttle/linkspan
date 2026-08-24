// Package vscode is the REST surface cs-bridge drives for VS Code Remote-SSH.
// The SSH server it starts lives in subsystems/sshd.
package vscode

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cyber-shuttle/linkspan/subsystems/sshd"
	"github.com/cyber-shuttle/linkspan/utils"
	gossh "golang.org/x/crypto/ssh"
)

const maxRequestBytes = 64 << 10

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
	// An ssh public key is well under a kilobyte; the cap stops a large body
	// from being read into memory. net/http closes the body itself.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if _, _, _, _, err := gossh.ParseAuthorizedKey([]byte(req.AuthorizedKey)); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "authorized_key is missing or invalid")
		return
	}

	port, err := utils.AvailablePort()
	if err != nil {
		utils.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The port is the identity: the client reads it back out of the id. Loopback
	// only -- it reaches clients through the tunnel, never the node's network.
	id := fmt.Sprintf("s-%d", port)
	sshd.Start(id, fmt.Sprintf("127.0.0.1:%d", port), req.AuthorizedKey)

	utils.RespondJSON(w, http.StatusCreated, SessionResponse{ID: id, BindPort: int32(port)})
}
