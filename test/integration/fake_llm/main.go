// Package main is a minimal fake OpenAI-compatible LLM server used in integration tests.
// It implements:
//   POST /v1/chat/completions  → returns a deterministic fake completion with token usage
//   GET  /v1/models            → returns a static model list
//
// The server validates the Authorization header and records every request so tests can
// assert that credentials were injected and traffic was routed correctly.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// requestLog is a thread-safe record of all received requests, exposed at GET /requests
// so test code can assert what arrived without needing a shared memory space.
type requestLog struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	Time          time.Time `json:"time"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Authorization string    `json:"authorization"` // full Bearer token value
	Model         string    `json:"model,omitempty"`
}

var log_ = &requestLog{}

func (r *requestLog) add(e logEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *requestLog) snapshot() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *requestLog) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/requests", handleRequests) // test helper: list received requests
	mux.HandleFunc("/reset", handleReset)        // test helper: clear request log
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("fake-llm listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("fake-llm: %v", err)
	}
}

func record(r *http.Request, model string) {
	auth := r.Header.Get("Authorization")
	// Redact everything after "Bearer " prefix for logging safety; keep first 8 chars.
	if strings.HasPrefix(auth, "Bearer ") {
		tok := strings.TrimPrefix(auth, "Bearer ")
		if len(tok) > 8 {
			tok = tok[:8] + "..."
		}
		auth = "Bearer " + tok
	}
	log_.add(logEntry{
		Time:          time.Now().UTC(),
		Method:        r.Method,
		Path:          r.URL.Path,
		Authorization: auth,
		Model:         model,
	})
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Decode request body to extract model name.
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "unknown"
	}

	record(r, req.Model)

	// Build a deterministic fake completion.
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-fake-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": fmt.Sprintf("fake response from %s", req.Model),
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 7,
			"total_tokens":      17,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	record(r, "")
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "gpt-4o", "object": "model", "owned_by": "fake-llm"},
			{"id": "gpt-4o-mini", "object": "model", "owned_by": "fake-llm"},
			{"id": "claude-3-5-sonnet", "object": "model", "owned_by": "fake-llm"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(log_.snapshot())
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log_.reset()
	w.WriteHeader(http.StatusNoContent)
}
