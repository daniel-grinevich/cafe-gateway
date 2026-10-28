package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// APIError is rendered as {"error": {"code": "<slug>", "message": "...", ...extra}}.
type APIError struct {
	Status  int
	Code    string
	Message string
	Headers map[string]string
	Extra   map[string]any
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func apiError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

func (e *APIError) with(k string, v any) *APIError {
	if e.Extra == nil {
		e.Extra = map[string]any{}
	}
	e.Extra[k] = v
	return e
}

func (e *APIError) header(k, v string) *APIError {
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	e.Headers[k] = v
	return e
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // JSON for API clients, never embedded in HTML: keep < > & readable
	if err := enc.Encode(body); err != nil {
		slog.Debug("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, e *APIError) {
	if info := requestInfoFrom(r.Context()); info != nil {
		info.code = e.Code
	}
	for k, v := range e.Headers {
		w.Header().Set(k, v)
	}
	body := map[string]any{"code": e.Code, "message": e.Message}
	for k, v := range e.Extra {
		body[k] = v
	}
	writeJSON(w, e.Status, map[string]any{"error": body})
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, apiError(http.StatusNotFound, "not_found", "Not found."))
}
