package transfer

import (
	"errors"
	"strings"
)

var (
	// ErrNotFound means no transfer matches the lookup.
	ErrNotFound = errors.New("transfer not found")

	// ErrInsufficientBalance means the balance doesn't cover the amount.
	// Nothing is stored, so the same idempotency key can be retried later.
	ErrInsufficientBalance = errors.New("insufficient balance")

	// ErrIdempotencyKeyReused means the key was already used for a different request.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")

	// ErrDuplicateIdempotencyKey is returned by TransferWriter.Create when the key is already taken.
	ErrDuplicateIdempotencyKey = errors.New("duplicate idempotency key")

	// ErrUnavailable means the store can't be reached. The request can be retried later.
	ErrUnavailable = errors.New("store unavailable")
)

// FieldError is an invalid request field, named as in the API.
type FieldError struct {
	Field   string
	Message string
}

// ValidationError lists all invalid fields of a request.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	names := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		names[i] = f.Field
	}
	return "invalid fields: " + strings.Join(names, ", ")
}
