package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/gorilla/mux"
)

// In-memory metadata store (key → arbitrary JSON value).
var (
	metadataStore = make(map[string]json.RawMessage)
	metadataMu    sync.RWMutex
)

// RegisterRoutes sets up all API routes on the given router.
func RegisterRoutes(api *mux.Router) {
	// Jupyter kernel management
	api.HandleFunc("/jupyter/kernels", jupyter.ListKernels).Methods("GET")
	api.HandleFunc("/jupyter/kernels", jupyter.ProvisionKernel).Methods("POST")
	api.HandleFunc("/jupyter/kernels/{id}", jupyter.DeleteKernel).Methods("DELETE")
	api.HandleFunc("/jupyter/kernels/{id}/connection", jupyter.GetKernelConnectionInfo).Methods("GET")
	api.HandleFunc("/jupyter/kernels/{id}/status", jupyter.GetKernelStatus).Methods("GET")
	api.HandleFunc("/jupyter/kernels/shutdown", jupyter.ShutdownKernel).Methods("POST")

	// VS Code remote session management
	api.HandleFunc("/vscode/sessions", vscode.ListVSCodeSessions).Methods("GET")
	api.HandleFunc("/vscode/sessions", vscode.CreateVSCodeSession).Methods("POST")
	api.HandleFunc("/vscode/sessions/{id}", vscode.DeleteVSCodeSession).Methods("DELETE")
	api.HandleFunc("/vscode/sessions/{id}/status", vscode.GetVSCodeSessionStatus).Methods("GET")

	// Tunnel management
	api.HandleFunc("/tunnels/devtunnels", tunnel.ListDevTunnels).Methods("GET")
	api.HandleFunc("/tunnels/devtunnels", tunnel.CreateDevTunnel).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/forward", tunnel.ForwardDevTunnelPort).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/auth-token", tunnel.RefreshDevTunnelAuthToken).Methods("POST")
	api.HandleFunc("/tunnels/devtunnels/{id}", tunnel.DeleteDevTunnel).Methods("DELETE")

	api.HandleFunc("/tunnels/frp", tunnel.ListFRPTunnels).Methods("GET")
	api.HandleFunc("/tunnels/frp", tunnel.CreateFRPTunnelProxy).Methods("POST")
	api.HandleFunc("/tunnels/frp/{id}", tunnel.DeleteFRPTunnel).Methods("DELETE")

	// Provider-agnostic tunnel endpoints
	// NOTE: /tunnels/connect must be registered before /tunnels/{id} so that
	// gorilla/mux does not match "connect" as a tunnel ID.
	api.HandleFunc("/tunnels/connect", tunnel.ConnectTunnel).Methods("POST")
	api.HandleFunc("/tunnels/connect/{id}", tunnel.DisconnectTunnel).Methods("DELETE")
	api.HandleFunc("/tunnels", tunnel.ListTunnels).Methods("GET")
	api.HandleFunc("/tunnels", tunnel.CreateTunnel).Methods("POST")
	api.HandleFunc("/tunnels/{id}/ports", tunnel.AddTunnelPort).Methods("POST")
	api.HandleFunc("/tunnels/{id}", tunnel.DeleteTunnel).Methods("DELETE")

	// Health and workflow status
	api.HandleFunc("/health", handleHealth).Methods("GET")

	// Metadata store — in-memory key-value for shared state
	api.HandleFunc("/metadata", handleMetadata).Methods("GET")
	api.HandleFunc("/metadata/{key:.+}", handleMetadataKey).Methods("GET", "PUT", "DELETE")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok"}`)
}

func handleMetadata(w http.ResponseWriter, r *http.Request) {
	metadataMu.RLock()
	defer metadataMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metadataStore)
}

func handleMetadataKey(w http.ResponseWriter, r *http.Request) {
	key := mux.Vars(r)["key"]
	switch r.Method {
	case "GET":
		metadataMu.RLock()
		val, ok := metadataStore[key]
		metadataMu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(val)
	case "PUT":
		r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		metadataMu.Lock()
		metadataStore[key] = json.RawMessage(body)
		metadataMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "DELETE":
		metadataMu.Lock()
		delete(metadataStore, key)
		metadataMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}
