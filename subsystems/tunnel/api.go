package tunnel

import (
	"encoding/json"
	"net/http"

	utils "github.com/cyber-shuttle/linkspan/utils"
)

type Tunnel struct {
	ID     string `json:"id"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
	Active bool   `json:"active"`
}

type DevTunnelCreateRequest struct {
	TunnelName string `json:"tunnelName"`
	Expiration string `json:"expiration"`
	Ports      []int  `json:"ports"`
	CreateToken bool   `json:"createToken"`
}

type DevTunnelCreateResponse struct {
	TunnelName string `json:"tunnelName"`
	TunnelID   string `json:"tunnelID"`
	Token	  string `json:"token,omitempty"`
}

type DevTunnelListResponse struct {
	DevTunnelInfos []*DevTunnelInfo `json:"tunnels"`
}

func ListDevTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := GlobalDevTunnelManager.GetAll()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	response := DevTunnelListResponse{
		DevTunnelInfos: tunnels,
	}
	utils.RespondJSON(w, http.StatusOK, response)
}

func CreateDevTunnel(w http.ResponseWriter, r *http.Request) {
	devTunnelCR := DevTunnelCreateRequest{}
	_ = json.NewDecoder(r.Body).Decode(&devTunnelCR)
	_ = r.Body.Close()
	
	info, err := DevTunnelCreate(devTunnelCR.TunnelName, devTunnelCR.Expiration, devTunnelCR.Ports)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	
	_, connection, err := DevTunnelHost(devTunnelCR.TunnelName, devTunnelCR.CreateToken)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	response := DevTunnelCreateResponse{
		TunnelName: devTunnelCR.TunnelName,
		TunnelID:   info.TunnelID,
		Token:      connection.Token,
	}

	utils.RespondJSON(w, http.StatusCreated, response)	
}

func CloseTunnel(w http.ResponseWriter, r *http.Request) {
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
