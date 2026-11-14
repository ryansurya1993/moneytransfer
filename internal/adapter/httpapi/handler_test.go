package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/adapter/httpapi"
	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type fakeService struct {
	submitResult transfer.SubmitResult
	getResult    transfer.Transfer
	err          error
	panicMsg     string

	gotReq      transfer.SubmitRequest
	gotDeadline bool
}

func (f *fakeService) Submit(ctx context.Context, req transfer.SubmitRequest) (transfer.SubmitResult, error) {
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	f.gotReq = req
	_, f.gotDeadline = ctx.Deadline()
	return f.submitResult, f.err
}

func (f *fakeService) Get(ctx context.Context, _ uuid.UUID) (transfer.Transfer, error) {
	_, f.gotDeadline = ctx.Deadline()
	return f.getResult, f.err
}

type fakePinger struct{ err error }

func (p fakePinger) PingContext(context.Context) error { return p.err }

var sample = transfer.Transfer{
	ID:                uuid.MustParse("0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f"),
	MerchantID:        1,
	Amount:            150000,
	Currency:          transfer.IDR,
	BankCode:          "BCA",
	AccountNumber:     "1234566789",
	AccountHolderName: "Budi Santoso",
	ReferenceID:       "INV-001",
	Description:       "Payout for invoice INV-001",
	Status:            transfer.StatusPending,
	CreatedAt:         time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC),
	UpdatedAt:         time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC),
}

const validBody = `{"amount":150000,"currency":"IDR","bank_code":"BCA","account_number":"1234566789",` +
	`"account_holder_name":"Budi Santoso","reference_id":"INV-001","description":"Payout for invoice INV-001"}`

type testServer struct {
	handler http.Handler
	svc     *fakeService
	logs    *bytes.Buffer
}

func newTestServer(svc *fakeService, ping error) testServer {
	logs := &bytes.Buffer{}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(logs, nil)))
	h := httpapi.NewHandler(svc, fakePinger{err: ping}, logger, httpapi.Options{MerchantID: 1, DBTimeout: time.Second})
	return testServer{handler: h, svc: svc, logs: logs}
}

func (s testServer) do(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

func newRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), method, path, body)
}

func postTransfer(t *testing.T, body string) *http.Request {
	t.Helper()
	r := newRequest(t, http.MethodPost, "/transfer", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "key-1")
	return r
}

type problemBody struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Code          string `json:"code"`
	CorrelationID string `json:"correlation_id"`
	Errors        []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem %q: %v", rec.Body.String(), err)
	}
	if p.Type != "about:blank" || p.Status != rec.Code || p.Title != http.StatusText(rec.Code) {
		t.Errorf("problem = %+v, want about:blank with status %d", p, rec.Code)
	}
	if p.CorrelationID == "" || p.CorrelationID != rec.Header().Get("X-Correlation-ID") {
		t.Errorf("correlation_id = %q, want the X-Correlation-ID header %q", p.CorrelationID, rec.Header().Get("X-Correlation-ID"))
	}
	return p
}

func TestSubmitAccepted(t *testing.T) {
	tests := []struct {
		name         string
		replayed     bool
		wantReplayed string
	}{
		{name: "new transfer", replayed: false, wantReplayed: ""},
		{name: "replay", replayed: true, wantReplayed: "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&fakeService{submitResult: transfer.SubmitResult{Transfer: sample, Replayed: tt.replayed}}, nil)

			rec := s.do(t, postTransfer(t, validBody))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Location"); got != "/transfer/"+sample.ID.String() {
				t.Errorf("Location = %q", got)
			}
			if got := rec.Header().Get("Idempotent-Replayed"); got != tt.wantReplayed {
				t.Errorf("Idempotent-Replayed = %q, want %q", got, tt.wantReplayed)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			want := `{"id":"0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f","status":"PENDING","amount":150000,"currency":"IDR",` +
				`"bank_code":"BCA","account_number":"****6789","account_holder_name":"Budi Santoso","reference_id":"INV-001",` +
				`"created_at":"2026-11-13T08:00:00Z","updated_at":"2026-11-13T08:00:00Z"}`
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("body\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestSubmitPassesRequestToService(t *testing.T) {
	s := newTestServer(&fakeService{submitResult: transfer.SubmitResult{Transfer: sample}}, nil)
	r := postTransfer(t, validBody)
	r.Header.Set("X-Correlation-ID", "trace-42")

	s.do(t, r)

	want := transfer.SubmitRequest{
		MerchantID:        1,
		IdempotencyKey:    "key-1",
		CorrelationID:     "trace-42",
		Amount:            150000,
		Currency:          transfer.IDR,
		BankCode:          "BCA",
		AccountNumber:     "1234566789",
		AccountHolderName: "Budi Santoso",
		ReferenceID:       "INV-001",
		Description:       "Payout for invoice INV-001",
	}
	if s.svc.gotReq != want {
		t.Errorf("request\n got %+v\nwant %+v", s.svc.gotReq, want)
	}
	if !s.svc.gotDeadline {
		t.Error("service called without a deadline")
	}
}

func TestSubmitResponseOmitsMissingReference(t *testing.T) {
	tr := sample
	tr.ReferenceID = ""
	s := newTestServer(&fakeService{submitResult: transfer.SubmitResult{Transfer: tr}}, nil)

	rec := s.do(t, postTransfer(t, validBody))

	if !strings.Contains(rec.Body.String(), `"reference_id":null`) {
		t.Errorf("body %s, want reference_id null", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "description") {
		t.Errorf("body %s echoes the description", rec.Body)
	}
}

func TestSubmitRejectsBody(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
		wantField   string
	}{
		{name: "wrong content type", contentType: "text/plain", body: validBody, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "missing content type", contentType: "", body: validBody, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "too large", contentType: "application/json", body: `{"description":"` + strings.Repeat("x", 17<<10) + `"}`, wantStatus: 413, wantCode: "PAYLOAD_TOO_LARGE"},
		{name: "empty", contentType: "application/json", body: "", wantStatus: 400, wantCode: "INVALID_JSON"},
		{name: "malformed", contentType: "application/json", body: `{"amount":`, wantStatus: 400, wantCode: "INVALID_JSON"},
		{name: "not an object", contentType: "application/json", body: `[1,2]`, wantStatus: 400, wantCode: "INVALID_JSON"},
		{name: "trailing data", contentType: "application/json", body: validBody + `{}`, wantStatus: 400, wantCode: "INVALID_JSON"},
		{name: "decimal amount", contentType: "application/json", body: `{"amount":1.5}`, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantField: "amount"},
		{name: "exponent amount", contentType: "application/json", body: `{"amount":1e3}`, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantField: "amount"},
		{name: "string amount", contentType: "application/json", body: `{"amount":"150000"}`, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantField: "amount"},
		{name: "numeric bank code", contentType: "application/json", body: `{"bank_code":14}`, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantField: "bank_code"},
		{name: "unknown field", contentType: "application/json", body: `{"amount_cents":150000}`, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantField: "amount_cents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&fakeService{}, nil)
			r := postTransfer(t, tt.body)
			r.Header.Set("Content-Type", tt.contentType)

			rec := s.do(t, r)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			p := decodeProblem(t, rec)
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
			if tt.wantField != "" && (len(p.Errors) != 1 || p.Errors[0].Field != tt.wantField) {
				t.Errorf("errors = %+v, want one for %q", p.Errors, tt.wantField)
			}
			if s.svc.gotReq != (transfer.SubmitRequest{}) {
				t.Error("service called for a rejected body")
			}
		})
	}
}

func TestSubmitAcceptsJSONWithCharset(t *testing.T) {
	s := newTestServer(&fakeService{submitResult: transfer.SubmitResult{Transfer: sample}}, nil)
	r := postTransfer(t, validBody)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	if rec := s.do(t, r); rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

func TestServiceErrors(t *testing.T) {
	validation := &transfer.ValidationError{Fields: []transfer.FieldError{
		{Field: "amount", Message: "must be greater than 0"},
		{Field: "idempotency_key", Message: "must be 1-255 characters"},
	}}
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantFields []string
		wantLog    string
	}{
		{name: "validation", err: validation, wantStatus: 400, wantCode: "VALIDATION_FAILED", wantFields: []string{"amount", "Idempotency-Key"}},
		{name: "insufficient balance", err: transfer.ErrInsufficientBalance, wantStatus: 422, wantCode: "INSUFFICIENT_BALANCE", wantLog: "transfer declined"},
		{name: "key reused", err: transfer.ErrIdempotencyKeyReused, wantStatus: 422, wantCode: "IDEMPOTENCY_KEY_REUSED"},
		{name: "store unreachable", err: fmt.Errorf("begin transaction: %w", transfer.ErrUnavailable), wantStatus: 503, wantCode: "SERVICE_UNAVAILABLE"},
		{name: "deadline", err: fmt.Errorf("debit: %w", context.DeadlineExceeded), wantStatus: 503, wantCode: "SERVICE_UNAVAILABLE"},
		{name: "unexpected", err: errors.New(`pq: relation "transfers" does not exist`), wantStatus: 500, wantCode: "INTERNAL_ERROR", wantLog: "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&fakeService{err: tt.err}, nil)

			rec := s.do(t, postTransfer(t, validBody))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			p := decodeProblem(t, rec)
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
			var fields []string
			for _, e := range p.Errors {
				fields = append(fields, e.Field)
			}
			if strings.Join(fields, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("fields = %v, want %v", fields, tt.wantFields)
			}
			if tt.wantStatus == 503 && rec.Header().Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
			}
			if strings.Contains(rec.Body.String(), "does not exist") {
				t.Errorf("body leaks the internal error: %s", rec.Body)
			}
			if !strings.Contains(s.logs.String(), tt.wantLog) {
				t.Errorf("logs %s, want %q", s.logs, tt.wantLog)
			}
		})
	}
}

func TestDeclineLogMasksAccountNumber(t *testing.T) {
	s := newTestServer(&fakeService{err: transfer.ErrInsufficientBalance}, nil)

	s.do(t, postTransfer(t, validBody))

	logs := s.logs.String()
	if !strings.Contains(logs, "****6789") || strings.Contains(logs, "1234566789") {
		t.Errorf("decline log must contain only the masked account number: %s", logs)
	}
	if strings.Contains(logs, "Budi") || strings.Contains(logs, "invoice") {
		t.Errorf("decline log contains personal data: %s", logs)
	}
}

func TestClientGoneIsNotAServerError(t *testing.T) {
	s := newTestServer(&fakeService{err: context.Canceled}, nil)

	rec := s.do(t, postTransfer(t, validBody))

	if rec.Code != 499 {
		t.Errorf("status = %d, want 499", rec.Code)
	}
	if strings.Contains(s.logs.String(), `"level":"ERROR"`) {
		t.Errorf("logged an error for a client that left: %s", s.logs)
	}
}

func TestGetTransfer(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		err        error
		wantStatus int
	}{
		{name: "found", path: "/transfer/" + sample.ID.String(), wantStatus: 200},
		{name: "not found", path: "/transfer/" + sample.ID.String(), err: transfer.ErrNotFound, wantStatus: 404},
		{name: "invalid id", path: "/transfer/not-a-uuid", wantStatus: 404},
		{name: "store unreachable", path: "/transfer/" + sample.ID.String(), err: transfer.ErrUnavailable, wantStatus: 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&fakeService{getResult: sample, err: tt.err}, nil)

			rec := s.do(t, newRequest(t, http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != 200 {
				decodeProblem(t, rec)
				return
			}
			if !strings.Contains(rec.Body.String(), `"account_number":"****6789"`) {
				t.Errorf("body %s, want masked account number", rec.Body)
			}
			if !s.svc.gotDeadline {
				t.Error("service called without a deadline")
			}
		})
	}
}

func TestRoutingErrorsUseProblemDetails(t *testing.T) {
	tests := []struct {
		method     string
		path       string
		wantStatus int
		wantAllow  string
	}{
		{method: http.MethodGet, path: "/nope", wantStatus: 404},
		{method: http.MethodGet, path: "/transfer/a/b", wantStatus: 404},
		{method: http.MethodGet, path: "/transfer", wantStatus: 405, wantAllow: "POST"},
		{method: http.MethodDelete, path: "/transfer/" + sample.ID.String(), wantStatus: 405, wantAllow: "GET, HEAD"},
		{method: http.MethodPost, path: "/healthz", wantStatus: 405, wantAllow: "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			s := newTestServer(&fakeService{}, nil)

			rec := s.do(t, newRequest(t, tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			decodeProblem(t, rec)
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
		})
	}
}

func TestCorrelationID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{name: "uuid kept", incoming: "0193f6e8-7b1c-7cc3-9a5e-4b4f1c2d3e4f", keep: true},
		{name: "dots and underscores kept", incoming: "req_1.a", keep: true},
		{name: "missing", incoming: "", keep: false},
		{name: "line break replaced", incoming: "abc\ninjected", keep: false},
		{name: "space replaced", incoming: "abc def", keep: false},
		{name: "too long replaced", incoming: strings.Repeat("a", 65), keep: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&fakeService{submitResult: transfer.SubmitResult{Transfer: sample}}, nil)
			r := postTransfer(t, validBody)
			r.Header.Set("X-Correlation-ID", tt.incoming)

			rec := s.do(t, r)

			got := rec.Header().Get("X-Correlation-ID")
			if tt.keep && got != tt.incoming {
				t.Errorf("X-Correlation-ID = %q, want %q", got, tt.incoming)
			}
			if !tt.keep {
				if _, err := uuid.Parse(got); err != nil {
					t.Errorf("X-Correlation-ID = %q, want a generated UUID", got)
				}
			}
			if s.svc.gotReq.CorrelationID != got {
				t.Errorf("service got %q, response header %q", s.svc.gotReq.CorrelationID, got)
			}
			if !strings.Contains(s.logs.String(), `"correlation_id":"`+got+`"`) {
				t.Errorf("access log %s, want correlation_id %q", s.logs, got)
			}
		})
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	s := newTestServer(&fakeService{panicMsg: "boom"}, nil)

	rec := s.do(t, postTransfer(t, validBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if p := decodeProblem(t, rec); p.Code != "INTERNAL_ERROR" {
		t.Errorf("code = %q, want INTERNAL_ERROR", p.Code)
	}
	if !strings.Contains(s.logs.String(), "boom") || !strings.Contains(s.logs.String(), "goroutine") {
		t.Errorf("logs %s, want the panic value and stack", s.logs)
	}
	if !strings.Contains(s.logs.String(), `"status":500`) {
		t.Errorf("access log %s, want status 500", s.logs)
	}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		path       string
		ping       error
		wantStatus int
	}{
		{path: "/healthz", ping: errors.New("down"), wantStatus: 200},
		{path: "/readyz", ping: nil, wantStatus: 200},
		{path: "/readyz", ping: errors.New("connection refused"), wantStatus: 503},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			s := newTestServer(&fakeService{}, tt.ping)

			rec := s.do(t, newRequest(t, http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body, _ := io.ReadAll(rec.Body)
			if tt.wantStatus == 200 && strings.TrimSpace(string(body)) != `{"status":"ok"}` {
				t.Errorf("body = %s", body)
			}
		})
	}
}
