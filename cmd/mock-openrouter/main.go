// Command mock-openrouter is a stand-in for OpenRouter in local development: canned decisions, deterministic
// embeddings and a tiny model catalogue. It never calls the real API.
//
//	OPENROUTER_BASE_URL=http://localhost:8090/api/v1
//	OPENROUTER_DECISIONS_URL=http://localhost:8090/api/alpha/decisions
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	srv := &http.Server{Addr: addr, Handler: Handler(), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("mock openrouter listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

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
			Questions map[string]map[string]any `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Questions) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": "questions required"}})
			return
		}
		answers := map[string]any{}
		for k, q := range req.Questions {
			switch q["type"] {
			case "choice":
				answers[k] = map[string]any{"type": "choice", "choice": 0, "probabilities": []float64{0.7, 0.3}, "confidence": 0.7}
			case "score":
				answers[k] = map[string]any{"type": "score", "score": 7, "confidence": 0.6}
			case "box":
				answers[k] = map[string]any{"type": "box", "box": []float64{0.1, 0.1, 0.5, 0.5}}
			default:
				answers[k] = map[string]any{"type": "noul", "noul": "yes"}
			}
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
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	return mux
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
