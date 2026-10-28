// Package mockopenrouter is a stand-in for OpenRouter: decisions validated against OpenRouter's published schema
// with canned answers of the right shape, deterministic embeddings, and a tiny model catalogue. It never calls the
// real API. Used by local compose (cmd/mock-openrouter) and by tests.
package mockopenrouter

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func authorized(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401, "message": "No auth credentials found"}})
		return false
	}
	return true
}

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/alpha/decisions", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var req struct {
			Model     string                    `json:"model"`
			State     any                       `json:"state"`
			Questions map[string]map[string]any `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, "Invalid JSON body")
			return
		}
		if req.State == nil || len(req.Questions) == 0 {
			badRequest(w, "state and questions are required")
			return
		}
		answers := map[string]any{}
		for _, k := range sortedKeys(req.Questions) {
			answer, problem := answer(req.Questions[k])
			if problem != "" {
				badRequest(w, "questions."+k+": "+problem)
				return
			}
			answers[k] = answer
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "mock-dec-" + time.Now().Format("150405.000"), "model": req.Model, "provider": "mock",
			"answers": answers,
			"usage":   map[string]any{"input_tokens": 120, "output_tokens": 8, "cost": 0.0004},
		})
	})
	mux.HandleFunc("POST /api/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var req struct {
			Model      string `json:"model"`
			Input      any    `json:"input"`
			Dimensions int    `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": "bad json"}})
			return
		}
		var inputs []string
		switch v := req.Input.(type) {
		case string:
			inputs = []string{v}
		case []any:
			for _, s := range v {
				str, _ := s.(string)
				inputs = append(inputs, str)
			}
		}
		dims := req.Dimensions
		if dims <= 0 {
			dims = 8
		}
		data := make([]any, len(inputs))
		tokens := 0
		for i, s := range inputs {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector(s, dims)}
			tokens += max(1, len(s)/4)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "mock-emb", "object": "list", "model": req.Model, "data": data,
			"usage": map[string]any{"prompt_tokens": tokens, "total_tokens": tokens, "cost": float64(tokens) * 0.00000001},
		})
	})
	models := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "perplexity/pplx-decider-flash-v1.2", "pricing": map[string]any{"prompt": "0.000001", "completion": "0.000004", "request": "0"}},
			map[string]any{"id": "qwen/qwen3-embedding-8b", "pricing": map[string]any{"prompt": "0.00000001", "completion": "0"}},
		}})
	}
	mux.HandleFunc("GET /api/v1/models", models)
	mux.HandleFunc("GET /api/v1/embeddings/models", models)
	// Key info, like GET https://openrouter.ai/api/v1/key: a $10 limit with plenty left.
	mux.HandleFunc("GET /api/v1/key", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"label": "sk-or-mock", "limit": 10, "limit_remaining": 9.5, "limit_reset": nil, "usage": 0.5,
			"usage_daily": 0.01, "usage_weekly": 0.1, "usage_monthly": 0.5, "is_free_tier": false,
		}})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	return mux
}

// badRequest mirrors OpenRouter's 400 body: {"error": {"code": 400, "message", "metadata"}}.
func badRequest(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"code": 400, "message": "Invalid request parameters", "metadata": map[string]any{"raw": detail},
	}})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func guidance(v any) bool {
	switch v := v.(type) {
	case string:
		return v != ""
	case map[string]any, []any:
		return true
	}
	return false
}

// answer validates one question like OpenRouter's schema (DecisionsRequest in https://openrouter.ai/openapi.json)
// and returns a canned answer of the matching shape, or a problem description.
func answer(q map[string]any) (map[string]any, string) {
	if !guidance(q["instructions"]) {
		return nil, "instructions is required (a string, object or array)"
	}
	switch q["type"] {
	case "noul":
		if c, present := q["criteria"]; present {
			m, ok := c.(map[string]any)
			if !ok || !guidance(m["true"]) || !guidance(m["false"]) {
				return nil, `criteria must be {"true": ..., "false": ...}`
			}
		}
		// The probability that the answer is true.
		return map[string]any{"type": "noul", "noul": 0.96}, ""
	case "choice":
		c, ok := q["criteria"].(map[string]any)
		if !ok || len(c) == 0 {
			return nil, `criteria must be an object of {"<option>": "<when to pick it>"}`
		}
		options := sortedKeys(c)
		probs := map[string]any{}
		for i, o := range options {
			probs[o] = 0.0
			if i == 0 {
				probs[o] = 1.0
			}
		}
		return map[string]any{"type": "choice", "choice": options[0], "confidence": 0.75, "probabilities": probs}, ""
	case "score":
		c, ok := q["criteria"].([]any)
		if !ok || len(c) == 0 {
			return nil, "criteria must be a non-empty array, ordered from lowest to highest"
		}
		legend, probs := map[string]any{}, map[string]any{}
		for i, item := range c {
			legend[strconv.Itoa(i)] = item
			probs[strconv.Itoa(i)] = 0.0
		}
		top := len(c) - 1
		probs[strconv.Itoa(top)] = 1.0
		return map[string]any{"type": "score", "score": float64(top), "confidence": 0.9, "legend": legend, "probabilities": probs}, ""
	case "box":
		return map[string]any{"type": "box", "box": map[string]any{"top": 0.1, "bottom": 0.6, "left": 0.2, "right": 0.7}, "confidence": 0.8}, ""
	}
	return nil, "type must be one of noul, choice, score, box"
}

// vector is a deterministic unit vector derived from the text.
func vector(s string, dims int) []float64 {
	out := make([]float64, dims)
	var norm float64
	for i := range out {
		h := sha256.Sum256([]byte(s + string(rune(i))))
		out[i] = float64(binary.BigEndian.Uint32(h[:4]))/math.MaxUint32*2 - 1
		norm += out[i] * out[i]
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] /= norm
	}
	return out
}
