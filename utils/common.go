package utils

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
)

// RespondJSON writes a JSON response with the given status code.
// Use this exported helper from other packages: `utils.RespondJSON`.
func RespondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// GetAvailablePort finds and returns an available TCP port.
// It works by binding to port 0, which lets the OS assign an available port,
// then closes the listener and returns the assigned port.
func GetAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("failed to find available port: %w", err)
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}
