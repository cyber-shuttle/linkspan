package vscode

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cyber-shuttle/linkspan/utils"
	gossh "golang.org/x/crypto/ssh"
)

type VSCodeSessionRequest struct {
	AuthorizedKey string `json:"authorized_key"` // the private half never leaves the caller
}

type VSCodeSessionResponse struct {
	ID       string `json:"id"`
	BindPort int32  `json:"bind_port"`
}

func ListVSCodeSessions(w http.ResponseWriter, r *http.Request) {
	sessions := listAllSessionStatuses()
	utils.RespondJSON(w, http.StatusOK, sessions)
}

func CreateVSCodeSession(w http.ResponseWriter, r *http.Request) {
	sessionReq := VSCodeSessionRequest{}
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

	// Generate a session ID (in production, use a proper ID generator)
	sessionID := fmt.Sprintf("s-%d", availablePort)

	// Loopback only: the port reaches clients through the tunnel, never the node's network.
	StartSSHServerForVSCodeConnection(sessionID, fmt.Sprintf("127.0.0.1:%d", availablePort), sessionReq.AuthorizedKey)

	utils.RespondJSON(w, http.StatusCreated, VSCodeSessionResponse{ID: sessionID, BindPort: int32(availablePort)})
}
