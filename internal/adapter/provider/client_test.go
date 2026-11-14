package provider_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/provider"
	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

var payout = transfer.Transfer{
	ID:                uuid.MustParse("0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f"),
	Amount:            150000,
	Currency:          transfer.IDR,
	BankCode:          "BCA",
	AccountNumber:     "1234566789",
	AccountHolderName: "Budi Santoso",
	Description:       "Payout for invoice INV-001",
	ReferenceID:       "INV-001",
}

func TestPayoutClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantSucceeded bool
		wantCode      transfer.FailureCode
		wantErr       bool
		wantAlert     bool
	}{
		{name: "succeeded", status: 200, body: `{"provider_reference":"p-1","status":"SUCCEEDED","failure_code":null}`, wantSucceeded: true},
		{name: "invalid account", status: 200, body: `{"provider_reference":"p-1","status":"FAILED","failure_code":"INVALID_ACCOUNT"}`, wantCode: transfer.FailureInvalidAccount},
		{name: "account closed", status: 200, body: `{"status":"FAILED","failure_code":"ACCOUNT_CLOSED"}`, wantCode: transfer.FailureAccountClosed},
		{name: "invalid bank code", status: 200, body: `{"status":"FAILED","failure_code":"INVALID_BANK_CODE"}`, wantCode: transfer.FailureInvalidBankCode},
		{name: "rejected by bank", status: 200, body: `{"status":"FAILED","failure_code":"REJECTED_BY_BANK"}`, wantCode: transfer.FailureRejectedByBank},
		{name: "unknown failure code", status: 200, body: `{"status":"FAILED","failure_code":"UNEXPECTED_BANK_RESPONSE"}`, wantErr: true, wantAlert: true},
		{name: "unknown status", status: 200, body: `{"provider_reference":"p-1","status":"PENDING_REVIEW"}`, wantErr: true, wantAlert: true},
		{name: "success without a reference", status: 200, body: `{"status":"SUCCEEDED"}`, wantErr: true, wantAlert: true},
		{name: "malformed JSON", status: 200, body: `{"status":`, wantErr: true},
		{name: "unavailable", status: 503, body: `{"error_code":"SERVICE_UNAVAILABLE"}`, wantErr: true},
		{name: "server error", status: 500, body: ``, wantErr: true},
		{name: "rate limited", status: 429, body: ``, wantErr: true},
		{name: "conflict", status: 409, body: `{"error_code":"IDEMPOTENCY_KEY_CONFLICT"}`, wantErr: true, wantAlert: true},
		{name: "bad request", status: 400, body: `{"error_code":"INVALID_REQUEST"}`, wantErr: true, wantAlert: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			var logs bytes.Buffer
			c := provider.NewClient(srv.URL, time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))

			res, err := c.Payout(t.Context(), payout)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if res.Succeeded != tt.wantSucceeded || res.FailureCode != tt.wantCode {
				t.Errorf("result %+v, want succeeded %v code %q", res, tt.wantSucceeded, tt.wantCode)
			}
			if got := strings.Contains(logs.String(), `"alert":"provider_unrecognized_response"`); got != tt.wantAlert {
				t.Errorf("alert logged = %v, want %v; logs %s", got, tt.wantAlert, logs.String())
			}
			if strings.Contains(logs.String(), payout.AccountNumber) || strings.Contains(logs.String(), "Budi") {
				t.Errorf("log contains personal data: %s", logs.String())
			}
		})
	}
}

func TestPayoutRequest(t *testing.T) {
	var got struct {
		method, path, key, correlation, contentType string
		body                                        map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.key, got.correlation = r.Header.Get("Idempotency-Key"), r.Header.Get("X-Correlation-ID")
		got.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = io.WriteString(w, `{"provider_reference":"p-1","status":"SUCCEEDED"}`)
	}))
	defer srv.Close()
	c := provider.NewClient(srv.URL, time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	if _, err := c.Payout(logging.WithCorrelationID(t.Context(), "corr-1"), payout); err != nil {
		t.Fatal(err)
	}

	if got.method != http.MethodPost || got.path != "/payouts" || got.contentType != "application/json" {
		t.Errorf("request %s %s %s, want POST /payouts JSON", got.method, got.path, got.contentType)
	}
	if got.key != payout.ID.String() || got.correlation != "corr-1" {
		t.Errorf("Idempotency-Key %q, X-Correlation-ID %q; want the transfer id and corr-1", got.key, got.correlation)
	}
	want := map[string]any{"amount": 150000.0, "currency": "IDR", "bank_code": "BCA", "account_number": "1234566789", "account_holder_name": "Budi Santoso"}
	if len(got.body) != len(want) {
		t.Errorf("body %v, want exactly %v (no description or reference)", got.body, want)
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, got.body[k], v)
		}
	}
}

func TestPayoutTimeoutIsAnError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release) // runs first, so Close doesn't wait on the stalled handler
	c := provider.NewClient(srv.URL, 50*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	start := time.Now()
	_, err := c.Payout(t.Context(), payout)

	if err == nil {
		t.Fatal("no error after the timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %v, want about 50ms", elapsed)
	}
}
