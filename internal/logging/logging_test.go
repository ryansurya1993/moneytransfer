package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
)

func TestHandlerAddsCorrelationID(t *testing.T) {
	withID := logging.WithCorrelationID(context.Background(), "req-1")

	tests := []struct {
		name string
		ctx  context.Context
		log  func(l *slog.Logger, ctx context.Context)
		want any
	}{
		{
			name: "context with id",
			ctx:  withID,
			log:  func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "hello") },
			want: "req-1",
		},
		{
			name: "logger derived with With",
			ctx:  withID,
			log:  func(l *slog.Logger, ctx context.Context) { l.With("k", "v").InfoContext(ctx, "hello") },
			want: "req-1",
		},
		{
			name: "context without id",
			ctx:  context.Background(),
			log:  func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "hello") },
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(logging.NewHandler(slog.NewJSONHandler(&buf, nil)))

			tt.log(logger, tt.ctx)

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("decode log line %q: %v", buf.String(), err)
			}
			if got := rec["correlation_id"]; got != tt.want {
				t.Errorf("correlation_id = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCorrelationIDMissing(t *testing.T) {
	if got := logging.CorrelationID(context.Background()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
