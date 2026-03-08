package tunnel

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

// GenericCreateRequest is the JSON body for POST /tunnels.
type GenericCreateRequest struct {
	Provider   string `json:"provider"`
	TunnelName string `json:"tunnelName,omitempty"`
	AuthToken  string `json:"authToken"`
	Ports      []int  `json:"ports,omitempty"`
	Expiration string `json:"expiration,omitempty"`
	ServerURL  string `json:"serverUrl,omitempty"`
}

// GenericCreateResponse is the JSON body returned after a provider-agnostic create.
type GenericCreateResponse struct {
	TunnelID      string `json:"tunnelId"`
	ConnectToken  string `json:"connectToken,omitempty"`
	ConnectionURL string `json:"connectionUrl,omitempty"`
	Provider      string `json:"provider"`
}

// GenericConnectRequest is the JSON body for POST /tunnels/connect.
type GenericConnectRequest struct {
	Provider string `json:"provider"`
	TunnelID string `json:"tunnelId"`
	Token    string `json:"token"`
}

// GenericConnectResponse is the JSON body returned after a provider-agnostic connect.
type GenericConnectResponse struct {
	ConnectionID string      `json:"connectionId"`
	PortMap      map[int]int `json:"portMap"`
}

// GenericAddPortRequest is the JSON body for POST /tunnels/{id}/ports.
type GenericAddPortRequest struct {
	Provider string `json:"provider"`
	Port     int    `json:"port"`
}

// CreateTunnel handles POST /tunnels (provider-agnostic).
func CreateTunnel(w http.ResponseWriter, r *http.Request) {
	var req GenericCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	if req.Provider == "" {
		req.Provider = "devtunnel"
	}

	p, err := GetProvider(req.Provider)
	if err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	result, err := p.Create(context.Background(), CreateOpts{
		Name:       req.TunnelName,
		AuthToken:  req.AuthToken,
		Ports:      req.Ports,
		Expiration: req.Expiration,
		ServerURL:  req.ServerURL,
	})
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	utils.RespondJSON(w, http.StatusCreated, GenericCreateResponse{
		TunnelID:      result.TunnelID,
		ConnectToken:  result.ConnectToken,
		ConnectionURL: result.ConnectionURL,
		Provider:      req.Provider,
	})
}

// ConnectTunnel handles POST /tunnels/connect (provider-agnostic).
func ConnectTunnel(w http.ResponseWriter, r *http.Request) {
	var req GenericConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	if req.Provider == "" {
		req.Provider = "devtunnel"
	}

	p, err := GetProvider(req.Provider)
	if err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	cr, err := p.Connect(context.Background(), req.TunnelID, req.Token)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	TrackConnection(cr.ConnectionID, req.Provider)

	utils.RespondJSON(w, http.StatusOK, GenericConnectResponse{
		ConnectionID: cr.ConnectionID,
		PortMap:      cr.PortMap,
	})
}

// DisconnectTunnel handles DELETE /tunnels/connect/{id} (provider-agnostic).
func DisconnectTunnel(w http.ResponseWriter, r *http.Request) {
	connID := mux.Vars(r)["id"]
	if connID == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "connection id required"})
		return
	}

	providerName, ok := ConnectionProvider(connID)
	if !ok {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	p, err := GetProvider(providerName)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := p.Disconnect(context.Background(), connID); err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	UntrackConnection(connID)
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// DeleteTunnel handles DELETE /tunnels/{id} (provider-agnostic).
func DeleteTunnel(w http.ResponseWriter, r *http.Request) {
	tunnelID := mux.Vars(r)["id"]
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "devtunnel"
	}

	p, err := GetProvider(provider)
	if err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := p.Delete(context.Background(), tunnelID); err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// ListTunnels handles GET /tunnels (provider-agnostic).
func ListTunnels(w http.ResponseWriter, r *http.Request) {
	var all []TunnelInfo
	providersMu.RLock()
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	providersMu.RUnlock()

	for _, name := range names {
		p, _ := GetProvider(name)
		if ts, err := p.List(context.Background()); err == nil {
			all = append(all, ts...)
		}
	}

	utils.RespondJSON(w, http.StatusOK, all)
}

// AddTunnelPort handles POST /tunnels/{id}/ports (provider-agnostic).
func AddTunnelPort(w http.ResponseWriter, r *http.Request) {
	tunnelID := mux.Vars(r)["id"]
	var req GenericAddPortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	if req.Provider == "" {
		req.Provider = "devtunnel"
	}

	p, err := GetProvider(req.Provider)
	if err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := p.AddPort(context.Background(), tunnelID, req.Port); err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
