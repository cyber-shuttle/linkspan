// Package utils holds the small helpers shared across linkspan's packages.
package utils

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
)

// RespondJSON writes v as a JSON response with the given status code.
func RespondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// RespondError writes {"error": msg} with the given status code.
func RespondError(w http.ResponseWriter, status int, msg string) {
	RespondJSON(w, status, map[string]string{"error": msg})
}

// AvailablePort binds port 0 so the OS assigns a free port, then releases it.
func AvailablePort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("failed to find available port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
