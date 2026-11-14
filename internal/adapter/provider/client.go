package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ryansurya1993/moneytransfer/internal/logging"
	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

const maxResponseBytes = 64 << 10

// failureCodes lists the provider's failure codes we treat as a definite refusal.
// Anything else leaves the outcome unknown: refunding a transfer the provider paid would pay twice.
var failureCodes = map[string]transfer.FailureCode{
	"INVALID_ACCOUNT":   transfer.FailureInvalidAccount,
	"ACCOUNT_CLOSED":    transfer.FailureAccountClosed,
	"INVALID_BANK_CODE": transfer.FailureInvalidBankCode,
	"REJECTED_BY_BANK":  transfer.FailureRejectedByBank,
}

var _ transfer.Provider = (*Client)(nil)

// Client calls the payout provider's HTTP API.
type Client struct {
	baseURL string
	http    *http.Client
	logger  *slog.Logger
}

// NewClient returns a Client for the provider at baseURL. Each call gives up after timeout.
func NewClient(baseURL string, timeout time.Duration, logger *slog.Logger) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}, logger: logger}
}

type payoutRequest struct {
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	BankCode          string `json:"bank_code"`
	AccountNumber     string `json:"account_number"`
	AccountHolderName string `json:"account_holder_name"`
}

type payoutResponse struct {
	ProviderReference string `json:"provider_reference"`
	Status            string `json:"status"`
	FailureCode       string `json:"failure_code"`
}

// Payout implements transfer.Provider.
func (c *Client) Payout(ctx context.Context, t transfer.Transfer) (transfer.PayoutResult, error) {
	body, err := json.Marshal(payoutRequest{
		Amount:            int64(t.Amount),
		Currency:          string(t.Currency),
		BankCode:          t.BankCode,
		AccountNumber:     t.AccountNumber,
		AccountHolderName: t.AccountHolderName,
	})
	if err != nil {
		return transfer.PayoutResult{}, fmt.Errorf("encode payout request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/payouts", bytes.NewReader(body))
	if err != nil {
		return transfer.PayoutResult{}, fmt.Errorf("build payout request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", t.ID.String())
	req.Header.Set("X-Correlation-ID", logging.CorrelationID(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return transfer.PayoutResult{}, fmt.Errorf("call provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return transfer.PayoutResult{}, fmt.Errorf("provider answered HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return transfer.PayoutResult{}, c.unrecognized(ctx, resp.StatusCode, "", "")
	}

	var r payoutResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&r); err != nil {
		return transfer.PayoutResult{}, fmt.Errorf("decode provider response: %w", err)
	}
	switch {
	case r.Status == "SUCCEEDED" && r.ProviderReference != "":
		return transfer.PayoutResult{Succeeded: true, ProviderReference: r.ProviderReference}, nil
	case r.Status == "FAILED" && failureCodes[r.FailureCode] != "":
		return transfer.PayoutResult{ProviderReference: r.ProviderReference, FailureCode: failureCodes[r.FailureCode]}, nil
	default:
		return transfer.PayoutResult{}, c.unrecognized(ctx, resp.StatusCode, r.Status, r.FailureCode)
	}
}

// unrecognized logs an answer that isn't a known success or failure and returns it as an error.
// The transfer is then retried, and if that doesn't help, left for a person to check.
func (c *Client) unrecognized(ctx context.Context, httpStatus int, status, code string) error {
	c.logger.WarnContext(ctx, "provider answer not recognized",
		"alert", "provider_unrecognized_response",
		"http_status", httpStatus,
		"provider_status", status,
		"provider_failure_code", code,
	)
	return fmt.Errorf("unrecognized provider answer: HTTP %d, status %q, failure code %q", httpStatus, status, code)
}
