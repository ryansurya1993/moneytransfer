package transfer_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const maxAmount transfer.Amount = 250_000_000

func validRequest() transfer.SubmitRequest {
	return transfer.SubmitRequest{
		MerchantID:        1,
		IdempotencyKey:    "key-1",
		CorrelationID:     "corr-1",
		Amount:            150_000,
		Currency:          transfer.IDR,
		BankCode:          "BCA",
		AccountNumber:     "0123456789",
		AccountHolderName: "Budi Santoso",
		ReferenceID:       "INV-001",
		Description:       "Payout for invoice INV-001",
	}
}

func TestSubmitAccepted(t *testing.T) {
	db := newFakeDB(1_000_000)
	svc := transfer.NewService(db, db, maxAmount)
	req := validRequest()

	res, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: unexpected error: %v", err)
	}

	got := res.Transfer
	if res.Replayed {
		t.Error("Replayed = true, want false")
	}
	if got.ID.Version() != 7 {
		t.Errorf("ID version = %d, want 7 (UUIDv7)", got.ID.Version())
	}
	if got.Status != transfer.StatusPending {
		t.Errorf("Status = %q, want %q", got.Status, transfer.StatusPending)
	}
	if got.Amount != req.Amount || got.AccountNumber != req.AccountNumber || got.CorrelationID != req.CorrelationID {
		t.Errorf("Transfer fields not copied from request: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want it set by the repository")
	}

	wantCalls := []string{"transfers.Create", "outbox.Add", "merchants.Debit", "ledger.Append"}
	if !slices.Equal(db.calls, wantCalls) {
		t.Errorf("call order = %v, want %v (the balance UPDATE must come after the inserts)", db.calls, wantCalls)
	}

	s := db.committed
	if s.balance != 850_000 || s.version != 1 {
		t.Errorf("balance, version = %d, %d, want 850000, 1", s.balance, s.version)
	}
	wantEvent := transfer.OutboxEvent{TransferID: got.ID, Type: transfer.EventTransferCreated, CorrelationID: "corr-1"}
	if !slices.Equal(s.outbox, []transfer.OutboxEvent{wantEvent}) {
		t.Errorf("outbox = %+v, want [%+v]", s.outbox, wantEvent)
	}
	wantEntry := transfer.LedgerEntry{
		MerchantID: 1, TransferID: got.ID, Type: transfer.EntryDebit,
		Amount: -150_000, BalanceAfter: 850_000, MerchantVersion: 1,
	}
	if !slices.Equal(s.ledger, []transfer.LedgerEntry{wantEntry}) {
		t.Errorf("ledger = %+v, want [%+v]", s.ledger, wantEntry)
	}
}

func TestSubmitInsufficientBalanceStoresNothing(t *testing.T) {
	db := newFakeDB(100_000)
	svc := transfer.NewService(db, db, maxAmount)

	_, err := svc.Submit(context.Background(), validRequest())
	if !errors.Is(err, transfer.ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	assertNothingStored(t, db, 100_000)
}

func TestSubmitDeclinedKeyIsEvaluatedAgain(t *testing.T) {
	// A declined request stores nothing, so retrying the same key can succeed later.
	db := newFakeDB(100_000)
	svc := transfer.NewService(db, db, maxAmount)
	req := validRequest()

	if _, err := svc.Submit(context.Background(), req); !errors.Is(err, transfer.ErrInsufficientBalance) {
		t.Fatalf("first Submit: err = %v, want ErrInsufficientBalance", err)
	}
	db.committed.balance = 1_000_000 // merchant tops up

	res, err := svc.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("retry Submit: unexpected error: %v", err)
	}
	if res.Replayed {
		t.Error("retry Replayed = true, want a new transfer")
	}
}

func TestSubmitReplaySameRequest(t *testing.T) {
	db := newFakeDB(1_000_000)
	svc := transfer.NewService(db, db, maxAmount)

	first, err := svc.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	retry := validRequest()
	retry.CorrelationID = "corr-2" // new on every retry, not part of the request
	second, err := svc.Submit(context.Background(), retry)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}

	if !second.Replayed {
		t.Error("Replayed = false, want true")
	}
	if second.Transfer.ID != first.Transfer.ID {
		t.Errorf("replayed ID = %v, want original %v", second.Transfer.ID, first.Transfer.ID)
	}
	if s := db.committed; s.balance != 850_000 || len(s.ledger) != 1 || len(s.outbox) != 1 || len(s.transfers) != 1 {
		t.Errorf("after replay: balance %d, %d ledger, %d outbox, %d transfers; want 850000, 1, 1, 1",
			s.balance, len(s.ledger), len(s.outbox), len(s.transfers))
	}
}

func TestSubmitKeyReusedWithDifferentRequest(t *testing.T) {
	tests := []struct {
		name   string
		change func(r *transfer.SubmitRequest)
	}{
		{"amount", func(r *transfer.SubmitRequest) { r.Amount++ }},
		{"account number", func(r *transfer.SubmitRequest) { r.AccountNumber = "9999999999" }},
		{"holder name", func(r *transfer.SubmitRequest) { r.AccountHolderName = "Siti Aminah" }},
		{"reference", func(r *transfer.SubmitRequest) { r.ReferenceID = "INV-002" }},
		{"description", func(r *transfer.SubmitRequest) { r.Description = "" }},
		{
			// Same characters, different field boundary.
			"field boundary", func(r *transfer.SubmitRequest) {
				r.BankCode, r.AccountNumber = "BCA0", "123456789"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newFakeDB(1_000_000)
			svc := transfer.NewService(db, db, maxAmount)
			if _, err := svc.Submit(context.Background(), validRequest()); err != nil {
				t.Fatalf("first Submit: %v", err)
			}

			req := validRequest()
			tt.change(&req)
			_, err := svc.Submit(context.Background(), req)
			if !errors.Is(err, transfer.ErrIdempotencyKeyReused) {
				t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
			}
			if db.committed.balance != 850_000 {
				t.Errorf("balance = %d, want 850000 (deducted once)", db.committed.balance)
			}
		})
	}
}

func TestSubmitFailedStepRollsBackEverything(t *testing.T) {
	injected := errors.New("injected failure")
	for _, step := range []string{"transfers.Create", "outbox.Add", "merchants.Debit", "ledger.Append"} {
		t.Run(step, func(t *testing.T) {
			db := newFakeDB(1_000_000)
			db.failAt, db.failErr = step, injected
			svc := transfer.NewService(db, db, maxAmount)

			_, err := svc.Submit(context.Background(), validRequest())
			if !errors.Is(err, injected) {
				t.Fatalf("err = %v, want the injected error", err)
			}
			assertNothingStored(t, db, 1_000_000)
		})
	}
}

func TestSubmitReturnsCommitError(t *testing.T) {
	// Nothing is stored when the commit fails, so Submit must not report success.
	commitErr := errors.New("commit failed")
	db := newFakeDB(1_000_000)
	db.commitErr = commitErr
	svc := transfer.NewService(db, db, maxAmount)

	res, err := svc.Submit(context.Background(), validRequest())
	if !errors.Is(err, commitErr) {
		t.Fatalf("err = %v, want the commit error", err)
	}
	if res.Transfer.ID != uuid.Nil {
		t.Errorf("result has transfer %v, want none", res.Transfer.ID)
	}
	assertNothingStored(t, db, 1_000_000)
}

func TestSubmitValidation(t *testing.T) {
	tests := []struct {
		name       string
		change     func(r *transfer.SubmitRequest)
		wantFields []string // nil = valid
	}{
		{"valid", func(*transfer.SubmitRequest) {}, nil},
		{"amount at max", func(r *transfer.SubmitRequest) { r.Amount = maxAmount }, nil},
		{"optional fields empty", func(r *transfer.SubmitRequest) { r.ReferenceID, r.Description = "", "" }, nil},
		{"holder name 100 multibyte chars", func(r *transfer.SubmitRequest) {
			r.AccountHolderName = strings.Repeat("é", 100)
		}, nil},

		{"amount zero", func(r *transfer.SubmitRequest) { r.Amount = 0 }, []string{"amount"}},
		{"amount negative", func(r *transfer.SubmitRequest) { r.Amount = -1 }, []string{"amount"}},
		{"amount over max", func(r *transfer.SubmitRequest) { r.Amount = maxAmount + 1 }, []string{"amount"}},
		{"currency missing", func(r *transfer.SubmitRequest) { r.Currency = "" }, []string{"currency"}},
		{"currency not IDR", func(r *transfer.SubmitRequest) { r.Currency = "USD" }, []string{"currency"}},
		{"bank code too short", func(r *transfer.SubmitRequest) { r.BankCode = "B" }, []string{"bank_code"}},
		{"bank code too long", func(r *transfer.SubmitRequest) { r.BankCode = strings.Repeat("B", 21) }, []string{"bank_code"}},
		{"bank code symbol", func(r *transfer.SubmitRequest) { r.BankCode = "BC-A" }, []string{"bank_code"}},
		{"account number too short", func(r *transfer.SubmitRequest) { r.AccountNumber = "1234" }, []string{"account_number"}},
		{"account number too long", func(r *transfer.SubmitRequest) { r.AccountNumber = strings.Repeat("1", 21) }, []string{"account_number"}},
		{"account number not digits", func(r *transfer.SubmitRequest) { r.AccountNumber = "12345a" }, []string{"account_number"}},
		{"holder name blank", func(r *transfer.SubmitRequest) { r.AccountHolderName = "   " }, []string{"account_holder_name"}},
		{"holder name too long", func(r *transfer.SubmitRequest) { r.AccountHolderName = strings.Repeat("a", 101) }, []string{"account_holder_name"}},
		{"reference too long", func(r *transfer.SubmitRequest) { r.ReferenceID = strings.Repeat("r", 65) }, []string{"reference_id"}},
		{"description too long", func(r *transfer.SubmitRequest) { r.Description = strings.Repeat("d", 256) }, []string{"description"}},
		{"zero byte in holder name", func(r *transfer.SubmitRequest) { r.AccountHolderName = "Budi\x00" }, []string{"account_holder_name"}},
		{"control character in reference", func(r *transfer.SubmitRequest) { r.ReferenceID = "INV\x1b[31m" }, []string{"reference_id"}},
		{"line break in description", func(r *transfer.SubmitRequest) { r.Description = "pay\nout" }, []string{"description"}},
		{"idempotency key missing", func(r *transfer.SubmitRequest) { r.IdempotencyKey = "" }, []string{"idempotency_key"}},
		{"idempotency key too long", func(r *transfer.SubmitRequest) { r.IdempotencyKey = strings.Repeat("k", 256) }, []string{"idempotency_key"}},
		{"all errors at once", func(r *transfer.SubmitRequest) {
			*r = transfer.SubmitRequest{MerchantID: 1, ReferenceID: strings.Repeat("r", 65)}
		}, []string{"amount", "currency", "bank_code", "account_number", "account_holder_name", "reference_id", "idempotency_key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newFakeDB(1_000_000_000)
			svc := transfer.NewService(db, db, maxAmount)
			req := validRequest()
			tt.change(&req)

			_, err := svc.Submit(context.Background(), req)

			if tt.wantFields == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *transfer.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			var got []string
			for _, f := range verr.Fields {
				got = append(got, f.Field)
			}
			if !slices.Equal(got, tt.wantFields) {
				t.Errorf("invalid fields = %v, want %v", got, tt.wantFields)
			}
			if len(db.calls) != 0 {
				t.Errorf("repository calls = %v, want none for an invalid request", db.calls)
			}
		})
	}
}

func TestGet(t *testing.T) {
	db := newFakeDB(1_000_000)
	svc := transfer.NewService(db, db, maxAmount)
	res, err := svc.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := svc.Get(context.Background(), res.Transfer.ID)
	if err != nil {
		t.Fatalf("Get existing: %v", err)
	}
	if got.ID != res.Transfer.ID {
		t.Errorf("Get ID = %v, want %v", got.ID, res.Transfer.ID)
	}

	if _, err := svc.Get(context.Background(), uuid.New()); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("Get unknown: err = %v, want ErrNotFound", err)
	}
}

func TestValidationErrorNamesFields(t *testing.T) {
	err := &transfer.ValidationError{Fields: []transfer.FieldError{{Field: "amount"}, {Field: "currency"}}}
	if got, want := err.Error(), "invalid fields: amount, currency"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// assertNothingStored checks that a failed submit left no trace.
func assertNothingStored(t *testing.T, db *fakeDB, wantBalance transfer.Amount) {
	t.Helper()
	s := db.committed
	if s.balance != wantBalance || s.version != 0 || len(s.transfers) != 0 || len(s.outbox) != 0 || len(s.ledger) != 0 {
		t.Errorf("state after failure: balance %d, version %d, %d transfers, %d outbox, %d ledger; want %d, 0, 0, 0, 0",
			s.balance, s.version, len(s.transfers), len(s.outbox), len(s.ledger), wantBalance)
	}
}
