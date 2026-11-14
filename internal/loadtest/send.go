package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// result is the answer to one request. A request without an answer has status 0.
type result struct {
	key      string
	status   int
	replayed bool
	latency  time.Duration
}

type traffic struct {
	results []result
	// skipped counts requests not sent because every slot was busy at their tick.
	skipped int
	elapsed time.Duration
}

// send fires the plan open loop: one shot every 1/rate, whether or not earlier requests have answered.
// Latency counts from the planned start, so a stalled api shows up in the percentiles
// instead of silently slowing the test down.
func send(ctx context.Context, client *http.Client, target string, shots []shot, rate, concurrency int) (traffic, error) {
	interval := time.Second / time.Duration(rate)
	slots := make(chan struct{}, concurrency)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
		t  traffic
	)
	timer := time.NewTimer(0)
	defer timer.Stop()

	start := time.Now()
	for i, s := range shots {
		at := start.Add(time.Duration(i) * interval)
		timer.Reset(time.Until(at))
		select {
		case <-ctx.Done():
			wg.Wait()
			return traffic{}, ctx.Err()
		case <-timer.C:
		}

		copies := 1
		if s.twin {
			copies = 2
		}
		for range copies {
			select {
			case slots <- struct{}{}:
			default:
				t.skipped++
				continue
			}
			wg.Go(func() {
				defer func() { <-slots }()
				r := post(ctx, client, target, s.req, at)
				mu.Lock()
				t.results = append(t.results, r)
				mu.Unlock()
			})
		}
	}
	t.elapsed = time.Since(start)
	wg.Wait()
	return t, nil
}

func post(ctx context.Context, client *http.Client, target string, req *request, planned time.Time) result {
	r := result{key: req.key}
	// A struct of strings and integers always encodes.
	body, _ := json.Marshal(req.body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/transfer", bytes.NewReader(body))
	if err != nil {
		return r
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.key)

	resp, err := client.Do(httpReq)
	r.latency = time.Since(planned)
	if err != nil {
		return r
	}
	defer func() { _ = resp.Body.Close() }()
	// Reading to the end lets the connection be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	r.status = resp.StatusCode
	r.replayed = resp.Header.Get("Idempotent-Replayed") == "true"
	return r
}
