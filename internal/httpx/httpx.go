// Ref: oga9-engineering/conventions/api-response-format.md
// Standard JSON envelope helpers — every API response goes through these
// (Rule 5). Error codes are snake_case machine strings, never sentences.
package httpx

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
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

// OptUUID parses an OPTIONAL uuid query/form value: empty means "no
// filter" (Nil), malformed non-empty is a 400. Path params should use a
// plain uuid.Parse + error check instead (chi always supplies a value).
func OptUUID(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	if raw == "" {
		return uuid.Nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	return id, true
}

// WritePage — paginated envelope: {"data": [...], "meta": {page, per_page, total}}
// per conventions/api-response-format.md.
func WritePage(w http.ResponseWriter, status int, data any, page, perPage int, total int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]any{"page": page, "per_page": perPage, "total": total},
	})
}

// PageParams parses ?page= & ?per_page= with defaults (page≥1, per_page
// default 25, capped at 200) — the convention's meta contract.
func PageParams(r *http.Request) (int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per < 1 {
		per = 25
	}
	if per > 200 {
		per = 200
	}
	return page, per
}
