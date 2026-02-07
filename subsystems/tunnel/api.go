package tunnel

import (
	"encoding/json"
	"net/http"

	utils "github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
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

type FrpTunnelProxyCreateRequest struct {
	TunnelName string `json:"tunnelName"`
	Port       int    `json:"port"`
	TunnelType string `json:"tunnelType"` // e.g., "xtcp"
	TunnelSecret string `json:"tunnelSecret"`
	DiscoveryHost string `json:"discoveryHost"`
	DiscoveryPort int    `json:"discoveryPort"`
	DiscoveryToken string `json:"discoveryToken"`
}


type FrpTunnelProxyCreateResponse struct {
	TunnelName string `json:"tunnelName"`
}

type FrpTunnelListResponse struct {
	FrpTunnelInfos []FrpTunnelInfo `json:"tunnels"`
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

func CloseDevTunnel(w http.ResponseWriter, r *http.Request) {
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

func CreateFrpTunnelProxy(w http.ResponseWriter, r *http.Request) {
	frpTunnelCr := FrpTunnelProxyCreateRequest{}
	_ = json.NewDecoder(r.Body).Decode(&frpTunnelCr)
	_ = r.Body.Close()

	info, err := FrpTunnelProxyCreate(frpTunnelCr.TunnelName, frpTunnelCr.Port, frpTunnelCr.TunnelType,
		frpTunnelCr.TunnelSecret, frpTunnelCr.DiscoveryHost, frpTunnelCr.DiscoveryPort, frpTunnelCr.DiscoveryToken)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	response := FrpTunnelProxyCreateResponse{
		TunnelName: info.TunnelName,
	}

	utils.RespondJSON(w, http.StatusCreated, response)
}

func ListFrpTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := FrpTunnelList()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	response := FrpTunnelListResponse{
		FrpTunnelInfos: tunnels,
	}

	utils.RespondJSON(w, http.StatusOK, response)
}

func GetFrpTunnelStatus(w http.ResponseWriter, r *http.Request) {
	// Implementation for getting FRP tunnel status
}

func TerminateFrpTunnel(w http.ResponseWriter, r *http.Request) {
	// Get session ID from query parameter or path
	vars := mux.Vars(r)
	tunnelName := vars["id"]
	if tunnelName == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "frp tunnel name required"})
		return
	}

	if err := StopFrpTunnelByName(tunnelName); err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusNoContent, nil)
}
