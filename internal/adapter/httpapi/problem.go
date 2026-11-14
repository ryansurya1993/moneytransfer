package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// maxBodyBytes is far above any valid transfer request; the limit only stops abuse.
const maxBodyBytes = 16 << 10

// statusClientClosedRequest is nginx's convention for a client that left before the response.
// Nobody receives it; it keeps those requests apart from server errors in the access log.
const statusClientClosedRequest = 499

// problem is an RFC 9457 problem details body. With type "about:blank" the title is the
// HTTP status text, and clients tell problems apart by code.
type problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail"`
	Code          string       `json:"code"`
	CorrelationID string       `json:"correlation_id"`
	Errors        []fieldError `json:"errors,omitempty"`
}

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, fields ...fieldError) {
	w.Header().Set("Content-Type", "application/problem+json")
	writeBody(w, status, problem{
		Type:          "about:blank",
		Title:         http.StatusText(status),
		Status:        status,
		Detail:        detail,
		Code:          code,
		CorrelationID: logging.CorrelationID(r.Context()),
		Errors:        fields,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	writeBody(w, status, v)
}

func writeBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The status line is already sent, so a failed write can't be reported to the client.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError maps an error from the service to a response. Only the expected domain errors
// reach the client; anything else is logged and answered with a generic 500.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *transfer.ValidationError
	switch {
	case errors.As(err, &verr):
		fields := make([]fieldError, len(verr.Fields))
		for i, f := range verr.Fields {
			fields[i] = fieldError{Field: f.Field, Message: f.Message}
			if f.Field == "idempotency_key" {
				fields[i].Field = "Idempotency-Key" // it comes from the header, not the body
			}
		}
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "The request has invalid fields.", fields...)
	case errors.Is(err, transfer.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "TRANSFER_NOT_FOUND", "No transfer exists with this ID.")
	case errors.Is(err, transfer.ErrInsufficientBalance):
		writeProblem(w, r, http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "The merchant balance doesn't cover the amount.")
	case errors.Is(err, transfer.ErrIdempotencyKeyReused):
		writeProblem(w, r, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED",
			"This Idempotency-Key was already used with a different request.")
	case errors.Is(err, transfer.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		h.logger.WarnContext(r.Context(), "database unavailable or too slow", "error", err)
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The service is temporarily unavailable. Retry the request later.")
	case errors.Is(err, context.Canceled):
		h.logger.InfoContext(r.Context(), "client closed the request")
		w.WriteHeader(statusClientClosedRequest)
	default:
		h.logger.ErrorContext(r.Context(), "request failed", "error", err)
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong on our side.")
	}
}

var errTrailingData = errors.New("data after the JSON object")

// decodeJSON reads a JSON object from the body into v.
// If the body isn't acceptable it writes the problem response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.")
		return false
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err = dec.Decode(v)
	if err == nil {
		// Decode stops after the first value; anything but the end of the body is a client bug.
		err = dec.Decode(&json.RawMessage{})
		if errors.Is(err, io.EOF) {
			return true
		}
		if err == nil {
			err = errTrailingData
		}
	}

	var (
		maxErr  *http.MaxBytesError
		typeErr *json.UnmarshalTypeError
	)
	invalidJSON := func(detail string) {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_JSON", detail)
	}
	switch {
	case errors.As(err, &maxErr):
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE",
			"The request body must not be larger than "+strconv.Itoa(maxBodyBytes)+" bytes.")
	case errors.Is(err, io.EOF):
		invalidJSON("The request body is empty.")
	case errors.Is(err, errTrailingData):
		invalidJSON("The request body must contain exactly one JSON object.")
	case errors.As(err, &typeErr) && typeErr.Field == "":
		invalidJSON("The request body must be a JSON object.")
	case errors.As(err, &typeErr):
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "The request has invalid fields.",
			fieldError{Field: typeErr.Field, Message: "must be " + jsonTypeName(typeErr.Type)})
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json has no error type for this, only the message.
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "The request has invalid fields.",
			fieldError{Field: field, Message: "is not a known field"})
	default:
		invalidJSON("The request body is not valid JSON.")
	}
	return false
}

func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int64:
		return "an integer"
	case reflect.String:
		return "a string"
	default:
		return "a " + t.Kind().String()
	}
}
