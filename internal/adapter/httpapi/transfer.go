package httpapi

import (
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

type transferRequest struct {
	// int64, so encoding/json itself rejects 1.5, 1e3 and "150000".
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	BankCode          string `json:"bank_code"`
	AccountNumber     string `json:"account_number"`
	AccountHolderName string `json:"account_holder_name"`
	ReferenceID       string `json:"reference_id"`
	Description       string `json:"description"`
}

// transferResponse lists every field a client may see. New domain fields stay private until
// added here, and the description is never echoed back.
type transferResponse struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	Amount            int64     `json:"amount"`
	Currency          string    `json:"currency"`
	BankCode          string    `json:"bank_code"`
	AccountNumber     string    `json:"account_number"`
	AccountHolderName string    `json:"account_holder_name"`
	ReferenceID       *string   `json:"reference_id"`
	FailureCode       *string   `json:"failure_code"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func newTransferResponse(t transfer.Transfer) transferResponse {
	resp := transferResponse{
		ID:                t.ID.String(),
		Status:            string(t.Status),
		Amount:            int64(t.Amount),
		Currency:          string(t.Currency),
		BankCode:          t.BankCode,
		AccountNumber:     maskAccountNumber(t.AccountNumber),
		AccountHolderName: t.AccountHolderName,
		CreatedAt:         t.CreatedAt.UTC(),
		UpdatedAt:         t.UpdatedAt.UTC(),
	}
	if t.ReferenceID != "" {
		resp.ReferenceID = &t.ReferenceID
	}
	if t.FailureCode != "" {
		code := string(t.FailureCode)
		resp.FailureCode = &code
	}
	return resp
}

type statusResponse struct {
	Status string `json:"status"`
}

// maskAccountNumber keeps the last four digits, enough for a person to recognize the account.
func maskAccountNumber(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}
