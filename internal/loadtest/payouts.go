package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// providerPayouts reads every payout the mock provider recorded, as status by idempotency key.
func providerPayouts(ctx context.Context, client *http.Client, providerURL string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, providerURL+"/stats", nil)
	if err != nil {
		return nil, fmt.Errorf("read provider payouts: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read provider payouts: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read provider payouts: status %d", resp.StatusCode)
	}

	var body struct {
		Payouts []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"payouts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode provider payouts: %w", err)
	}
	payouts := make(map[string]string, len(body.Payouts))
	for _, p := range body.Payouts {
		payouts[p.Key] = p.Status
	}
	return payouts, nil
}
