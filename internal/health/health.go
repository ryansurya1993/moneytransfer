package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const readyTimeout = time.Second

type response struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// NewHandler serves /healthz, failing while alive returns an error, and /readyz, failing while ready does.
// Keep alive to the process's own progress: a dependency outage shouldn't restart a healthy process.
func NewHandler(alive func() error, ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		write(w, alive())
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		write(w, ready(ctx))
	})
	return mux
}

func write(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(response{Status: "unavailable", Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(response{Status: "ok"})
}
