package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
)

const correlationHeader = "X-Correlation-ID"

// withCorrelationID uses the caller's correlation ID if it looks like one, otherwise a new one.
// The value lands in every log line and in the database, so it must not carry line breaks or bloat.
func withCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(correlationHeader)
		if !isValidCorrelationID(id) {
			id = uuid.NewString()
		}
		w.Header().Set(correlationHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithCorrelationID(r.Context(), id)))
	})
}

func isValidCorrelationID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range []byte(id) {
		ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// withAccessLog logs one line per request. Bodies are never logged: they hold account numbers.
func (h *handler) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		// The container healthcheck calls /healthz every few seconds; only a failed probe is news.
		if r.URL.Path == "/healthz" && sw.status == http.StatusOK {
			level = slog.LevelDebug
		}
		h.logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withRecover turns a panic into a logged 500 so one bad request doesn't take the process down.
func (h *handler) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			// http.ErrAbortHandler is how a handler asks the server to drop the connection.
			if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(p)
			}
			h.logger.ErrorContext(r.Context(), "panic serving request", "panic", p, "stack", string(debug.Stack()))
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong on our side.")
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
