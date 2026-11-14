package mockprovider

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Options sets the delays and how often each outcome happens.
// Calls not taken by a percentage succeed.
type Options struct {
	MinDelay           time.Duration
	MaxDelay           time.Duration
	FailurePercent     int
	TimeoutPercent     int
	UnavailablePercent int
	HangDuration       time.Duration
}

const (
	statusSucceeded = "SUCCEEDED"
	statusFailed    = "FAILED"
)

var knownFailureCodes = []string{"INVALID_ACCOUNT", "ACCOUNT_CLOSED", "INVALID_BANK_CODE", "REJECTED_BY_BANK"}

type outcome string

const (
	outcomeSucceeded   outcome = "succeeded"
	outcomeFailed      outcome = "failed"
	outcomeTimeout     outcome = "timeout_paid"
	outcomeUnavailable outcome = "unavailable"
)

// magicOutcomes lets a caller force an outcome with the last four digits of the account number.
// UNEXPECTED_BANK_RESPONSE is a failure code no client is expected to know.
var magicOutcomes = map[string]struct {
	outcome     outcome
	failureCode string
}{
	"0001": {outcomeFailed, "INVALID_ACCOUNT"},
	"0002": {outcome: outcomeTimeout},
	"0003": {outcome: outcomeUnavailable},
	"0004": {outcomeFailed, "UNEXPECTED_BANK_RESPONSE"},
}

type payoutRequest struct {
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	BankCode          string `json:"bank_code"`
	AccountNumber     string `json:"account_number"`
	AccountHolderName string `json:"account_holder_name"`
}

type payoutResponse struct {
	ProviderReference string  `json:"provider_reference"`
	Status            string  `json:"status"`
	FailureCode       *string `json:"failure_code"`
}

type errorResponse struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

type payout struct {
	req      payoutRequest
	resp     payoutResponse
	requests int
}

type handler struct {
	opts   Options
	logger *slog.Logger

	mu      sync.Mutex
	payouts map[string]*payout // by idempotency key
}

// NewHandler returns the provider's routes: POST /payouts, GET /payouts/{key} and GET /stats.
func NewHandler(opts Options, logger *slog.Logger) http.Handler {
	h := &handler{opts: opts, logger: logger, payouts: make(map[string]*payout)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /payouts", h.createPayout)
	mux.HandleFunc("GET /payouts/{key}", h.getPayout)
	mux.HandleFunc("GET /stats", h.stats)
	return h.withAccessLog(mux)
}

// createPayout records the outcome before the delay, like a bank that pays at once but answers slowly.
// A caller that gives up waiting may already be paid.
// A retry with the same key gets the recorded result instead of a second payout.
func (h *handler) createPayout(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "The Idempotency-Key header is required.")
		return
	}
	var req payoutRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil || req.Amount <= 0 || !isDigits(req.AccountNumber) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "The body needs a positive amount and a numeric account number.")
		return
	}

	h.mu.Lock()
	if p, ok := h.payouts[key]; ok {
		same, resp := p.req == req, p.resp
		if same {
			p.requests++
		}
		h.mu.Unlock()
		if !same {
			w.Header().Set("X-Mock-Outcome", "conflict")
			writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", "This key was used with a different request.")
			return
		}
		w.Header().Set("X-Mock-Outcome", "replay")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	o, code := h.decide(req.AccountNumber)
	w.Header().Set("X-Mock-Outcome", string(o))
	if o == outcomeUnavailable {
		h.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Try again later.")
		return
	}
	resp := payoutResponse{ProviderReference: "mock-" + uuid.NewString(), Status: statusSucceeded}
	if o == outcomeFailed {
		resp.Status, resp.FailureCode = statusFailed, &code
	}
	h.payouts[key] = &payout{req: req, resp: resp, requests: 1}
	h.mu.Unlock()

	delay := h.delay()
	if o == outcomeTimeout {
		delay = h.opts.HangDuration
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		writeJSON(w, http.StatusOK, resp)
	case <-r.Context().Done():
		// The caller gave up. The payout stands, as it would at a real bank.
	}
}

func (h *handler) getPayout(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	p, ok := h.payouts[r.PathValue("key")]
	var resp payoutResponse
	if ok {
		resp = p.resp
	}
	h.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "No payout exists for this key.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type statsEntry struct {
	Key         string  `json:"key"`
	Status      string  `json:"status"`
	FailureCode *string `json:"failure_code"`
	Requests    int     `json:"requests"`
}

func (h *handler) stats(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	entries := make([]statsEntry, 0, len(h.payouts))
	for key, p := range h.payouts {
		entries = append(entries, statsEntry{Key: key, Status: p.resp.Status, FailureCode: p.resp.FailureCode, Requests: p.requests})
	}
	h.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	writeJSON(w, http.StatusOK, struct {
		Payouts []statsEntry `json:"payouts"`
	}{entries})
}

// decide picks the outcome for a new payout, and the failure code if it fails.
func (h *handler) decide(account string) (outcome, string) {
	if m, ok := magicOutcomes[account[max(len(account)-4, 0):]]; ok {
		return m.outcome, m.failureCode
	}
	n := rand.IntN(100)
	switch {
	case n < h.opts.FailurePercent:
		return outcomeFailed, knownFailureCodes[rand.IntN(len(knownFailureCodes))]
	case n < h.opts.FailurePercent+h.opts.TimeoutPercent:
		return outcomeTimeout, ""
	case n < h.opts.FailurePercent+h.opts.TimeoutPercent+h.opts.UnavailablePercent:
		return outcomeUnavailable, ""
	default:
		return outcomeSucceeded, ""
	}
}

func (h *handler) delay() time.Duration {
	d := h.opts.MinDelay
	if span := h.opts.MaxDelay - h.opts.MinDelay; span > 0 {
		d += rand.N(span)
	}
	return d
}

// withAccessLog logs one line per request.
// X-Mock-Outcome says what the mock chose, which a real provider would never reveal.
func (h *handler) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			// Nothing was written: the caller left first (nginx logs this as 499).
			status = 499
		}
		h.logger.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"idempotency_key", r.Header.Get("Idempotency-Key"),
			"outcome", w.Header().Get("X-Mock-Outcome"),
		)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{ErrorCode: code, Message: message})
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
