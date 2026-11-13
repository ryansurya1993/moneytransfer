package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SubmitRequest is a merchant's request to send money to a bank account.
type SubmitRequest struct {
	MerchantID     MerchantID
	IdempotencyKey string
	CorrelationID  string

	Amount            Amount
	Currency          Currency
	BankCode          string
	AccountNumber     string
	AccountHolderName string
	ReferenceID       string
	Description       string
}

// validate returns all field errors at once.
func (r SubmitRequest) validate(maxAmount Amount) error {
	var fields []FieldError
	add := func(field, msg string) {
		fields = append(fields, FieldError{Field: field, Message: msg})
	}

	switch {
	case r.Amount <= 0:
		add("amount", "must be greater than 0")
	case r.Amount > maxAmount:
		add("amount", "must not exceed "+strconv.FormatInt(int64(maxAmount), 10))
	}
	if r.Currency != IDR {
		add("currency", "must be IDR")
	}
	if n := len(r.BankCode); n < 2 || n > 20 || !isASCIIAlnum(r.BankCode) {
		add("bank_code", "must be 2-20 letters or digits")
	}
	if n := len(r.AccountNumber); n < 5 || n > 20 || !isASCIIDigits(r.AccountNumber) {
		add("account_number", "must be 5-20 digits")
	}
	if strings.TrimSpace(r.AccountHolderName) == "" || utf8.RuneCountInString(r.AccountHolderName) > 100 {
		add("account_holder_name", "must be 1-100 characters and not blank")
	}
	if utf8.RuneCountInString(r.ReferenceID) > 64 {
		add("reference_id", "must be at most 64 characters")
	}
	if utf8.RuneCountInString(r.Description) > 255 {
		add("description", "must be at most 255 characters")
	}
	if n := utf8.RuneCountInString(r.IdempotencyKey); n < 1 || n > 255 {
		add("idempotency_key", "must be 1-255 characters")
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// hash identifies the request content, to detect a key reused for a different request.
// Fields are length-prefixed so "AB"+"123" and "AB1"+"23" don't collide.
// The correlation ID is left out because it changes on every retry.
func (r SubmitRequest) hash() string {
	h := sha256.New()
	for _, f := range []string{
		strconv.FormatInt(int64(r.Amount), 10),
		string(r.Currency),
		r.BankCode,
		r.AccountNumber,
		r.AccountHolderName,
		r.ReferenceID,
		r.Description,
	} {
		h.Write([]byte(strconv.Itoa(len(f)) + ":" + f + ";"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isASCIIAlnum(s string) bool {
	for _, c := range []byte(s) {
		isLetter := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		isDigit := '0' <= c && c <= '9'
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func isASCIIDigits(s string) bool {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
