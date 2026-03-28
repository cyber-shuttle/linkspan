package vscode

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

type VSCodeSessionRequest struct {
	MountUserHome bool `json:"mount_user_home"`
}

type VSCodeSessionResponse struct {
	ID       string `json:"id"`
	BindPort int32  `json:"bind_port"`
	Password string `json:"password,omitempty"` // Password is only returned on creation, not in session status or list responses.
}

func ListVSCodeSessions(w http.ResponseWriter, r *http.Request) {
	sessions := listAllSessions()
	utils.RespondJSON(w, http.StatusOK, sessions)
}

func CreateVSCodeSession(w http.ResponseWriter, r *http.Request) {
	sessionReq := VSCodeSessionRequest{}
	if err := json.NewDecoder(r.Body).Decode(&sessionReq); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	availablePort, err := utils.GetAvailablePort()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Generate a session ID (in production, use a proper ID generator)
	sessionID := fmt.Sprintf("s-%d", availablePort)
	password := utils.GenerateRandomPassword(16)

	StartSSHServerForVSCodeConnection(sessionID, fmt.Sprintf(":%d", availablePort), password)

	s := VSCodeSessionResponse{ID: sessionID, BindPort: int32(availablePort), Password: password}
	utils.RespondJSON(w, http.StatusCreated, s)
}

func DeleteVSCodeSession(w http.ResponseWriter, r *http.Request) {
	// Get session ID from query parameter or path
	vars := mux.Vars(r)
	sessionID := vars["id"]
	if sessionID == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "session id required"})
		return
	}

	if err := stopSSHServerBySessionID(sessionID); err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

func GetVSCodeSessionStatus(w http.ResponseWriter, r *http.Request) {
	// Get session ID from query parameter or path
	vars := mux.Vars(r)
	sessionID := vars["id"]
	if sessionID == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "session id required"})
		return
	}

	status, err := getSessionStatus(sessionID)
	if err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusOK, status)
}
