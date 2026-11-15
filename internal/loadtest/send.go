package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// Every progressEvery it writes how far it is to progress.
func send(ctx context.Context, client *http.Client, target string, shots []shot, rate, concurrency int,
	progress io.Writer, progressEvery time.Duration,
) (traffic, error) {
	interval := time.Second / time.Duration(rate)
	planned := time.Duration(len(shots)) * interval
	slots := make(chan struct{}, concurrency)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
		t  traffic
	)
	timer := time.NewTimer(0)
	defer timer.Stop()

	start := time.Now()
	lastProgress := start
	launched := 0
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
			launched++
			wg.Go(func() {
				defer func() { <-slots }()
				r := post(ctx, client, target, s.req, at)
				mu.Lock()
				t.results = append(t.results, r)
				mu.Unlock()
			})
		}
		// Checked between ticks rather than on its own timer, so reporting never delays a shot.
		if now := time.Now(); now.Sub(lastProgress) >= progressEvery {
			lastProgress = now
			_, _ = fmt.Fprintf(progress, "  sending %s/%s, %d sent\n",
				now.Sub(start).Round(time.Second), planned.Round(time.Second), launched)
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
