package vscode

import (
	"net/http"

	utils "github.com/cyber-shuttle/linkspan/utils"
)

type VSCodeSession struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	Status string `json:"status"`
}

func ListVSCodes(w http.ResponseWriter, r *http.Request) {
	sessions := []VSCodeSession{{ID: "s1", User: "alice", Status: "active"}}
	utils.RespondJSON(w, http.StatusOK, sessions)
}

func CreateVSCodeSession(w http.ResponseWriter, r *http.Request) {
	s := VSCodeSession{ID: "s-new", User: "bob", Status: "starting"}
	utils.RespondJSON(w, http.StatusCreated, s)
}

func TerminateVSCodeSession(w http.ResponseWriter, r *http.Request) {
	// placeholder: terminate session by id
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
