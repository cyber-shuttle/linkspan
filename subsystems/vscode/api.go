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

type SessionRequest struct {
	AuthorizedKey string `json:"authorized_key"` // the private half never leaves the caller
}

type SessionResponse struct {
	ID       string `json:"id"`
	BindPort int32  `json:"bind_port"`
}

func ListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := sshd.Statuses()
	utils.RespondJSON(w, http.StatusOK, sessions)
}

func CreateSession(w http.ResponseWriter, r *http.Request) {
	sessionReq := SessionRequest{}
	if err := json.NewDecoder(r.Body).Decode(&sessionReq); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	if _, _, _, _, err := gossh.ParseAuthorizedKey([]byte(sessionReq.AuthorizedKey)); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "authorized_key is missing or invalid"})
		return
	}

	availablePort, err := utils.GetAvailablePort()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// The port is the identity: it is unique while the server is up, and it is
	// what the client reads back out of the id to reach the sshd.
	sessionID := fmt.Sprintf("s-%d", availablePort)

	// Loopback only: the port reaches clients through the tunnel, never the node's network.
	sshd.Start(sessionID, fmt.Sprintf("127.0.0.1:%d", availablePort), sessionReq.AuthorizedKey)

	utils.RespondJSON(w, http.StatusCreated, SessionResponse{ID: sessionID, BindPort: int32(availablePort)})
}
