package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// Service is the part of the transfer service the API calls.
type Service interface {
	Submit(ctx context.Context, req transfer.SubmitRequest) (transfer.SubmitResult, error)
	Get(ctx context.Context, id uuid.UUID) (transfer.Transfer, error)
}

// Pinger checks that the database is reachable.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Options configures the handler.
type Options struct {
	// MerchantID is the merchant every request acts for, since the API has no authentication.
	MerchantID transfer.MerchantID
	// DBTimeout bounds the database work of one request. Hitting it gives 503.
	DBTimeout time.Duration
}

const readyTimeout = time.Second

type handler struct {
	svc    Service
	db     Pinger
	logger *slog.Logger
	opts   Options
}

// NewHandler returns the API routes wrapped in the correlation ID, access log and recovery middleware.
func NewHandler(svc Service, db Pinger, logger *slog.Logger, opts Options) http.Handler {
	h := &handler{svc: svc, db: db, logger: logger, opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /transfer", h.submit)
	mux.HandleFunc("GET /transfer/{id}", h.get)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)

	// ServeMux answers unknown paths and wrong methods in plain text. A pattern without a method
	// loses to one with a method on the same path, so these only catch what the routes above don't.
	mux.Handle("/transfer", methodNotAllowed(http.MethodPost))
	mux.Handle("/transfer/{id}", methodNotAllowed(http.MethodGet, http.MethodHead))
	mux.Handle("/healthz", methodNotAllowed(http.MethodGet, http.MethodHead))
	mux.Handle("/readyz", methodNotAllowed(http.MethodGet, http.MethodHead))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "No resource exists at this path.")
	})

	return withCorrelationID(h.withAccessLog(h.withRecover(mux)))
}

func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	var body transferRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.opts.DBTimeout)
	defer cancel()
	res, err := h.svc.Submit(ctx, transfer.SubmitRequest{
		MerchantID:        h.opts.MerchantID,
		IdempotencyKey:    r.Header.Get("Idempotency-Key"),
		CorrelationID:     logging.CorrelationID(r.Context()),
		Amount:            transfer.Amount(body.Amount),
		Currency:          transfer.Currency(body.Currency),
		BankCode:          body.BankCode,
		AccountNumber:     body.AccountNumber,
		AccountHolderName: body.AccountHolderName,
		ReferenceID:       body.ReferenceID,
		Description:       body.Description,
	})
	if errors.Is(err, transfer.ErrInsufficientBalance) {
		// Declines store nothing, so this log line is their only record.
		h.logger.InfoContext(r.Context(), "transfer declined",
			"reason", "insufficient_balance",
			"merchant_id", h.opts.MerchantID,
			"amount", body.Amount,
			"account_number", maskAccountNumber(body.AccountNumber),
		)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	t := res.Transfer
	w.Header().Set("Location", "/transfer/"+t.ID.String())
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, newTransferResponse(t))
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, transfer.ErrNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.opts.DBTimeout)
	defer cancel()
	t, err := h.svc.Get(ctx, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newTransferResponse(t))
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		h.logger.WarnContext(r.Context(), "database not ready", "error", err)
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The database is not reachable.")
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func methodNotAllowed(allowed ...string) http.Handler {
	allow := strings.Join(allowed, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeProblem(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Use one of the methods in the Allow header.")
	})
}
