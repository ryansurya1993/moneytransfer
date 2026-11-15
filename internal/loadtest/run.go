package loadtest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

// Options configures a run.
type Options struct {
	TargetURL        string
	ProviderURL      string
	Rate             int
	Duration         time.Duration
	Concurrency      int
	DuplicatePercent int
	MagicPercent     int
	RequestTimeout   time.Duration
	DrainTimeout     time.Duration
	MinAmount        int64
	MaxAmount        int64
	MerchantID       int64
}

const drainPoll = time.Second

// Run sends the burst, waits until the run's transfers have an outcome and checks the money.
// Other activity on the merchant during the run is allowed: the checks tell it apart through the ledger.
// It returns an error when the run could not be judged, for example when the provider can't be read.
func Run(ctx context.Context, db *sqlx.DB, o Options) (Report, error) {
	st := store{db: db, merchantID: o.MerchantID}
	start, err := st.snapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	if start.Held {
		return Report{}, fmt.Errorf("merchant %d payouts are on hold until its ledger findings are resolved; every request would be declined", o.MerchantID)
	}
	// Below this, almost every request would be declined, which proves little.
	if minBalance := 10 * (o.MinAmount + o.MaxAmount) / 2; start.Balance < minBalance {
		return Report{}, fmt.Errorf("balance %d is too low for a meaningful run (want at least %d); top up with `make topup`",
			start.Balance, minBalance)
	}

	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	runID := fmt.Sprintf("%s-%04x", time.Now().UTC().Format("20060102T150405Z"), rng.IntN(1<<16))
	ref := runReference(runID)
	shots := newPlan(rng, planOptions{
		runID:            runID,
		ticks:            max(int(o.Duration*time.Duration(o.Rate)/time.Second), 1),
		duplicatePercent: o.DuplicatePercent,
		magicPercent:     o.MagicPercent,
		minAmount:        o.MinAmount,
		maxAmount:        o.MaxAmount,
	})

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = o.Concurrency
	client := &http.Client{Timeout: o.RequestTimeout, Transport: transport}
	tr, err := send(ctx, client, o.TargetURL, shots, o.Rate, o.Concurrency)
	if err != nil {
		return Report{}, err
	}

	drained, drainTime, err := drain(ctx, st, ref, o.DrainTimeout)
	if err != nil {
		return Report{}, err
	}
	end, err := st.snapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	window, err := st.ledger(ctx, start.Version, end.Version, ref)
	if err != nil {
		return Report{}, err
	}
	ts, err := st.runTransfers(ctx, ref)
	if err != nil {
		return Report{}, err
	}
	// Read after the transfers: the provider records an outcome before it answers, so every outcome
	// stored above is already in its list.
	payouts, err := providerPayouts(ctx, client, o.ProviderURL)
	if err != nil {
		return Report{}, err
	}

	return Report{
		runID:        runID,
		options:      o,
		startBalance: start.Balance,
		traffic:      tr,
		drainTime:    drainTime,
		transfers:    ts,
		otherChanges: window.Other,
		checks: []check{
			drained,
			checkNotNegative(end),
			checkLedger(start, end, window),
			checkTransferEntries(ts),
			checkOneTransferPerKey(groupKeys(tr.results), ts),
			checkProvider(ts, payouts),
			checkEventsPublished(ts),
		},
	}, nil
}

// drain waits until none of the run's transfers is PENDING or PROCESSING and the relay has marked
// all their events published.
func drain(ctx context.Context, st store, ref string, timeout time.Duration) (check, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		inFlight, unpublished, err := st.unsettled(ctx, ref)
		if err != nil {
			return check{}, 0, err
		}
		waited := time.Since(start)
		finished := check{name: "all transfers finished", passed: true, detail: "none left PENDING or PROCESSING"}
		if inFlight == 0 && unpublished == 0 {
			return finished, waited, nil
		}
		if time.Now().After(deadline) {
			// Events still unpublished fail their own check.
			if inFlight == 0 {
				return finished, waited, nil
			}
			return check{
				name:   finished.name,
				detail: fmt.Sprintf("%d still PENDING or PROCESSING after %s", inFlight, timeout),
			}, waited, nil
		}
		select {
		case <-ctx.Done():
			return check{}, 0, ctx.Err()
		case <-time.After(drainPoll):
		}
	}
}
