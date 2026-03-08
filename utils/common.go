package utils

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
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

func FindLineInStdout(stdout string, searchString string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, searchString) {
			rest := strings.TrimPrefix(line, searchString)
			if rest != "" {
				// rest should be the path
				return strings.TrimSpace(rest), nil
			}
		}
	}
	return "", fmt.Errorf("line not found in stdout")
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
