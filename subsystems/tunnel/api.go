package tunnel

import (
	"encoding/json"
	"net/http"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	utils "github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

// Tunnel is the generic tunnel descriptor exposed by the REST API.
type Tunnel struct {
	ID     string `json:"id"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
	Active bool   `json:"active"`
}

// DevTunnelCreateRequest is the JSON body for POST /tunnels/devtunnel.
type DevTunnelCreateRequest struct {
	TunnelName string `json:"tunnelName"`
	Expiration string `json:"expiration"`
	// AuthToken is the Microsoft Entra ID (Azure AD) bearer token used to
	// authenticate against the Dev Tunnels service.  It is required for all
	// devtunnel operations.
	AuthToken  string `json:"authToken"`
	ServerPort int    `json:"serverPort"` // linkspan HTTP port to forward immediately
}

// DevTunnelCreateResponse is the JSON body returned after a successful create+host.
type DevTunnelCreateResponse struct {
	TunnelName    string `json:"tunnelName"`
	TunnelID      string `json:"tunnelID"`
	ConnectionURL string `json:"connectionURL,omitempty"`
	Token         string `json:"token,omitempty"`
}

// FrpTunnelProxyCreateRequest is the JSON body for POST /tunnels/frp.
type FrpTunnelProxyCreateRequest struct {
	TunnelName    string `json:"tunnelName"`
	Port          int    `json:"port"`
	TunnelType    string `json:"tunnelType"` // e.g. "xtcp"
	TunnelSecret  string `json:"tunnelSecret"`
	DiscoveryHost string `json:"discoveryHost"`
	DiscoveryPort int    `json:"discoveryPort"`
	DiscoveryToken string `json:"discoveryToken"`
}

// FrpTunnelProxyCreateResponse is the JSON body returned after FRP proxy creation.
type FrpTunnelProxyCreateResponse struct {
	TunnelName string `json:"tunnelName"`
}

// FrpTunnelListResponse is the JSON body for GET /tunnels/frp.
type FrpTunnelListResponse struct {
	FrpTunnelInfos []FrpTunnelInfo `json:"tunnels"`
}

// DevTunnelListResponse is the JSON body for GET /tunnels/devtunnel.
type DevTunnelListResponse struct {
	DevTunnelInfos []*DevTunnelInfo `json:"tunnels"`
}

// ListDevTunnels handles GET /tunnels/devtunnel.
func ListDevTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := GlobalDevTunnelManager.GetAll()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusOK, DevTunnelListResponse{DevTunnelInfos: tunnels})
}

// CreateDevTunnel handles POST /tunnels/devtunnel.
// Creates the tunnel, hosts the relay, and forwards the server port so the
// client can communicate with linkspan immediately.
func CreateDevTunnel(w http.ResponseWriter, r *http.Request) {
	var req DevTunnelCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	if req.AuthToken == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "authToken is required"})
		return
	}

	conn, err := DevTunnelCreate(req.TunnelName, req.Expiration, req.AuthToken, req.ServerPort)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusCreated, DevTunnelCreateResponse{
		TunnelName:    req.TunnelName,
		TunnelID:      conn.DevTunnelInfo.TunnelID,
		ConnectionURL: conn.ConnectionURL,
		Token:         conn.Token,
	})
}

// CloseDevTunnel handles DELETE /tunnels/devtunnel/{id}.
// It kills the host CLI process, deletes the tunnel via the SDK, and removes
// it from the in-memory manager.
func CloseDevTunnel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	tunnelName := vars["id"]
	if tunnelName == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "tunnel name required"})
		return
	}

	info, err := GlobalDevTunnelManager.Find(tunnelName)
	if err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	// Kill the host CLI process if one is running.
	if info.HostCmdID != "" {
		_ = pm.GlobalProcessManager.Kill(info.HostCmdID)
	}

	// Delete the tunnel on the service.
	if err := DevTunnelDelete(tunnelName, info.AuthToken); err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	GlobalDevTunnelManager.Remove(tunnelName)

	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// CreateFrpTunnelProxy handles POST /tunnels/frp.
func CreateFrpTunnelProxy(w http.ResponseWriter, r *http.Request) {
	var req FrpTunnelProxyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	info, err := FrpTunnelProxyCreate(
		req.TunnelName, req.Port, req.TunnelType,
		req.TunnelSecret, req.DiscoveryHost, req.DiscoveryPort, req.DiscoveryToken,
	)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusCreated, FrpTunnelProxyCreateResponse{
		TunnelName: info.TunnelName,
	})
}

// ListFrpTunnels handles GET /tunnels/frp.
func ListFrpTunnels(w http.ResponseWriter, r *http.Request) {
	ts, err := FrpTunnelList()
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusOK, FrpTunnelListResponse{FrpTunnelInfos: ts})
}

// TerminateFrpTunnel handles DELETE /tunnels/frp/{id}.
func TerminateFrpTunnel(w http.ResponseWriter, r *http.Request) {
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
