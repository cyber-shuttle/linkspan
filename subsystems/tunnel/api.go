package tunnel

import (
	"net/http"

	utils "github.com/cyber-shuttle/conduit/utils"
)

type Tunnel struct {
	ID     string `json:"id"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
	Active bool   `json:"active"`
}

func ListTunnels(w http.ResponseWriter, r *http.Request) {
	t := []Tunnel{{ID: "t1", Local: "127.0.0.1:8888", Remote: "10.1.1.2:22", Active: true}}
	utils.RespondJSON(w, http.StatusOK, t)
}

func CreateTunnel(w http.ResponseWriter, r *http.Request) {
	tunnel := Tunnel{ID: "t-new", Local: "127.0.0.1:9000", Remote: "10.1.1.3:9000", Active: true}
	utils.RespondJSON(w, http.StatusCreated, tunnel)
}

func CloseTunnel(w http.ResponseWriter, r *http.Request) {
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
