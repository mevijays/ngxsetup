package webui

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeActionResult is the common shape for every endpoint that runs a
// captured, potentially-mutating action: what it printed (the same
// transcript the CLI would have shown), and the error if it failed.
func writeActionResult(w http.ResponseWriter, output string, err error, data any) {
	resp := map[string]any{"output": output}
	if data != nil {
		resp["data"] = data
	}
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
