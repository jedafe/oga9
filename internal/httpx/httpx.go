// Ref: oga9-engineering/conventions/api-response-format.md
// Standard JSON envelope helpers — every API response goes through these
// (Rule 5). Error codes are snake_case machine strings, never sentences.
package httpx

import (
	"encoding/json"
	"net/http"
)

func WriteData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func WriteErr(w http.ResponseWriter, status int, code, msg string) {
	WriteErrDetails(w, status, code, msg, nil)
}

func WriteErrDetails(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := map[string]any{"code": code, "message": msg, "status": status}
	if details != nil {
		e["details"] = details
	}
	json.NewEncoder(w).Encode(map[string]any{"error": e})
}

// Decode reads a JSON body with a 1 MiB cap — enough for any current route.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}
