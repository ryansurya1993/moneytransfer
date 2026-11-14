package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ryansurya1993/moneytransfer/internal/health"
)

func TestHandler(t *testing.T) {
	stalled := errors.New("no round for 2m")
	down := errors.New("database unreachable")
	tests := []struct {
		name       string
		path       string
		alive      error
		ready      error
		wantStatus int
		wantBody   string
	}{
		{name: "alive", path: "/healthz", wantStatus: 200, wantBody: `{"status":"ok"}`},
		{name: "stalled", path: "/healthz", alive: stalled, wantStatus: 503, wantBody: `{"status":"unavailable","error":"no round for 2m"}`},
		{name: "alive while a dependency is down", path: "/healthz", ready: down, wantStatus: 200, wantBody: `{"status":"ok"}`},
		{name: "ready", path: "/readyz", wantStatus: 200, wantBody: `{"status":"ok"}`},
		{name: "not ready", path: "/readyz", ready: down, wantStatus: 503, wantBody: `{"status":"unavailable","error":"database unreachable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotDeadline bool
			h := health.NewHandler(
				func() error { return tt.alive },
				func(ctx context.Context) error { _, gotDeadline = ctx.Deadline(); return tt.ready },
			)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus || strings.TrimSpace(rec.Body.String()) != tt.wantBody {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, tt.wantStatus, tt.wantBody)
			}
			if tt.path == "/readyz" && !gotDeadline {
				t.Error("readiness check ran without a deadline")
			}
		})
	}
}
