package mockprovider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/mockprovider"
)

// noRandomness makes every non-magic call succeed at once.
var noRandomness = mockprovider.Options{HangDuration: time.Second}

func newHandler(opts mockprovider.Options) http.Handler {
	return mockprovider.NewHandler(opts, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func payoutBody(account string) string {
	return `{"amount":150000,"currency":"IDR","bank_code":"BCA","account_number":"` + account + `","account_holder_name":"Budi Santoso"}`
}

func post(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/payouts", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

type payoutResponse struct {
	ProviderReference string  `json:"provider_reference"`
	Status            string  `json:"status"`
	FailureCode       *string `json:"failure_code"`
	ErrorCode         string  `json:"error_code"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) payoutResponse {
	t.Helper()
	var p payoutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return p
}

type stats struct {
	Payouts []struct {
		Key         string  `json:"key"`
		Status      string  `json:"status"`
		FailureCode *string `json:"failure_code"`
		Requests    int     `json:"requests"`
	} `json:"payouts"`
}

func getStats(t *testing.T, h http.Handler) stats {
	t.Helper()
	var s stats
	if err := json.Unmarshal(get(t, h, "/stats").Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func code(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		opts       mockprovider.Options
		account    string
		wantStatus int
		wantPayout string
		wantCode   string
		wantError  string
	}{
		{name: "success", opts: noRandomness, account: "1234566789", wantStatus: 200, wantPayout: "SUCCEEDED"},
		{name: "magic invalid account", opts: noRandomness, account: "1234560001", wantStatus: 200, wantPayout: "FAILED", wantCode: "INVALID_ACCOUNT"},
		{name: "magic unavailable", opts: noRandomness, account: "1234560003", wantStatus: 503, wantError: "SERVICE_UNAVAILABLE"},
		{name: "magic unexpected code", opts: noRandomness, account: "1234560004", wantStatus: 200, wantPayout: "FAILED", wantCode: "UNEXPECTED_BANK_RESPONSE"},
		{name: "random failure", opts: mockprovider.Options{FailurePercent: 100}, account: "1234566789", wantStatus: 200, wantPayout: "FAILED"},
		{name: "random unavailable", opts: mockprovider.Options{UnavailablePercent: 100}, account: "1234566789", wantStatus: 503, wantError: "SERVICE_UNAVAILABLE"},
		{name: "random timeout answers after the hang", opts: mockprovider.Options{TimeoutPercent: 100, HangDuration: time.Millisecond}, account: "1234566789", wantStatus: 200, wantPayout: "SUCCEEDED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandler(tt.opts)

			rec := post(t, h, "key-1", payoutBody(tt.account))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			p := decode(t, rec)
			if p.Status != tt.wantPayout || p.ErrorCode != tt.wantError {
				t.Errorf("got status %q error %q, want %q %q", p.Status, p.ErrorCode, tt.wantPayout, tt.wantError)
			}
			if tt.wantCode != "" && code(p.FailureCode) != tt.wantCode {
				t.Errorf("failure_code = %q, want %q", code(p.FailureCode), tt.wantCode)
			}
			if tt.wantPayout == "FAILED" && code(p.FailureCode) == "" {
				t.Error("failed payout without a failure_code")
			}
			if tt.wantPayout != "" && !strings.HasPrefix(p.ProviderReference, "mock-") {
				t.Errorf("provider_reference = %q", p.ProviderReference)
			}
		})
	}
}

func TestUnavailableRecordsNothing(t *testing.T) {
	h := newHandler(noRandomness)

	post(t, h, "key-1", payoutBody("1234560003"))
	post(t, h, "key-1", payoutBody("1234560003"))

	if s := getStats(t, h); len(s.Payouts) != 0 {
		t.Errorf("stats = %+v, want no payouts", s.Payouts)
	}
}

func TestReplayReturnsStoredResult(t *testing.T) {
	h := newHandler(mockprovider.Options{FailurePercent: 50, HangDuration: time.Second})
	first := decode(t, post(t, h, "key-1", payoutBody("1234566789")))

	for range 5 {
		again := post(t, h, "key-1", payoutBody("1234566789"))
		if got := decode(t, again); got.ProviderReference != first.ProviderReference || got.Status != first.Status {
			t.Fatalf("replay = %+v, want %+v", got, first)
		}
		if got := again.Header().Get("X-Mock-Outcome"); got != "replay" {
			t.Errorf("X-Mock-Outcome = %q, want replay", got)
		}
	}

	s := getStats(t, h)
	if len(s.Payouts) != 1 || s.Payouts[0].Key != "key-1" || s.Payouts[0].Requests != 6 {
		t.Errorf("stats = %+v, want key-1 with 6 requests", s.Payouts)
	}
}

func TestKeyReusedWithDifferentBody(t *testing.T) {
	h := newHandler(noRandomness)
	post(t, h, "key-1", payoutBody("1234566789"))

	rec := post(t, h, "key-1", payoutBody("9876543210"))

	if rec.Code != http.StatusConflict || decode(t, rec).ErrorCode != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Errorf("got %d %s, want 409 IDEMPOTENCY_KEY_CONFLICT", rec.Code, rec.Body)
	}
	if s := getStats(t, h); s.Payouts[0].Requests != 1 {
		t.Errorf("requests = %d, want 1: a conflicting call is not a request for this payout", s.Payouts[0].Requests)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		body      string
		wantError string
	}{
		{name: "missing key", key: "", body: payoutBody("1234566789"), wantError: "MISSING_IDEMPOTENCY_KEY"},
		{name: "malformed json", key: "k", body: `{"amount":`, wantError: "INVALID_REQUEST"},
		{name: "zero amount", key: "k", body: `{"amount":0,"account_number":"1234566789"}`, wantError: "INVALID_REQUEST"},
		{name: "letters in account", key: "k", body: `{"amount":1,"account_number":"12ab"}`, wantError: "INVALID_REQUEST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, newHandler(noRandomness), tt.key, tt.body)

			if rec.Code != http.StatusBadRequest || decode(t, rec).ErrorCode != tt.wantError {
				t.Errorf("got %d %s, want 400 %s", rec.Code, rec.Body, tt.wantError)
			}
		})
	}
}

// A caller that times out may already have been paid, so a timeout must never count as a failure.
func TestTimedOutCallerIsStillPaid(t *testing.T) {
	var logs bytes.Buffer
	h := mockprovider.NewHandler(noRandomness, slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/payouts", strings.NewReader(payoutBody("1234560002")))
	r.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, r)

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("handler kept hanging %v after the caller left", elapsed)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("answered a caller that left: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), `"status":499`) {
		t.Errorf("access log %s, want status 499", logs.String())
	}
	retry := decode(t, post(t, h, "key-1", payoutBody("1234560002")))
	if retry.Status != "SUCCEEDED" {
		t.Errorf("retry status = %q, want SUCCEEDED", retry.Status)
	}
}

func TestConcurrentDuplicatesPayOnce(t *testing.T) {
	h := newHandler(mockprovider.Options{MinDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond, FailurePercent: 50, HangDuration: time.Second})

	refs := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			// No t.Fatal here: it must not be called from another goroutine.
			var p payoutResponse
			_ = json.Unmarshal(post(t, h, "key-1", payoutBody("1234566789")).Body.Bytes(), &p)
			refs <- p.ProviderReference
		})
	}
	wg.Wait()
	close(refs)

	first := <-refs
	for ref := range refs {
		if ref == "" || ref != first {
			t.Fatalf("got references %q and %q for one key", first, ref)
		}
	}
	if s := getStats(t, h); len(s.Payouts) != 1 || s.Payouts[0].Requests != 20 {
		t.Errorf("stats = %+v, want one payout with 20 requests", s.Payouts)
	}
}

func TestDelay(t *testing.T) {
	h := newHandler(mockprovider.Options{MinDelay: 30 * time.Millisecond, MaxDelay: 40 * time.Millisecond, HangDuration: time.Second})

	start := time.Now()
	post(t, h, "key-1", payoutBody("1234566789"))

	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("answered after %v, want at least 30ms", elapsed)
	}
}

func TestGetPayout(t *testing.T) {
	h := newHandler(noRandomness)
	created := decode(t, post(t, h, "key-1", payoutBody("1234566789")))

	rec := get(t, h, "/payouts/key-1")
	if rec.Code != http.StatusOK || decode(t, rec).ProviderReference != created.ProviderReference {
		t.Errorf("got %d %s, want the created payout", rec.Code, rec.Body)
	}
	if rec := get(t, h, "/payouts/other"); rec.Code != http.StatusNotFound || decode(t, rec).ErrorCode != "NOT_FOUND" {
		t.Errorf("got %d %s, want 404 NOT_FOUND", rec.Code, rec.Body)
	}
}

func TestStatsSortedByKey(t *testing.T) {
	h := newHandler(noRandomness)
	for _, key := range []string{"c", "a", "b"} {
		post(t, h, key, payoutBody("1234566789"))
	}

	s := getStats(t, h)

	var keys []string
	for _, p := range s.Payouts {
		keys = append(keys, p.Key)
	}
	if strings.Join(keys, ",") != "a,b,c" {
		t.Errorf("keys = %v, want a,b,c", keys)
	}
}
