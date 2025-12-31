package utils

import (
	"encoding/json"
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
