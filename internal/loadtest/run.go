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
// It returns an error when the run could not happen, for example because other transfers are
// still in flight and would move the balance during the run.
func Run(ctx context.Context, db *sqlx.DB, o Options) (Report, error) {
	st := store{db: db, merchantID: o.MerchantID}
	busy, err := st.inFlight(ctx)
	if err != nil {
		return Report{}, err
	}
	if busy > 0 {
		return Report{}, fmt.Errorf("merchant %d has transfers still PENDING or PROCESSING (%d); "+
			"wait until they finish, or they would move the balance during the run", o.MerchantID, busy)
	}

	start, err := st.snapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	// Below this, almost every request would be declined, which proves little.
	if minBalance := 10 * (o.MinAmount + o.MaxAmount) / 2; start.Balance < minBalance {
		return Report{}, fmt.Errorf("balance %d is too low for a meaningful run (want at least %d); top up with `make topup`",
			start.Balance, minBalance)
	}

	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	runID := fmt.Sprintf("%s-%04x", time.Now().UTC().Format("20060102T150405Z"), rng.IntN(1<<16))
	prefix := referencePrefix(runID)
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

	drained, drainTime, err := drain(ctx, st, prefix, o.DrainTimeout)
	if err != nil {
		return Report{}, err
	}
	end, err := st.snapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	ts, err := st.runTransfers(ctx, prefix)
	if err != nil {
		return Report{}, err
	}
	foreign, err := st.foreignEntries(ctx, start.LastEntryID, prefix)
	if err != nil {
		return Report{}, err
	}

	r := Report{
		runID:        runID,
		options:      o,
		startBalance: start.Balance,
		traffic:      tr,
		drainTime:    drainTime,
		transfers:    ts,
		checks: []check{
			drained,
			checkNotNegative(end),
			checkBalanceDrop(start, end, ts),
			checkOneTransferPerKey(groupKeys(tr.results), ts),
		},
	}
	if foreign > 0 {
		r.invalid = fmt.Sprintf("%d balance changes during the run did not come from its transfers", foreign)
	}
	return r, nil
}

// drain waits until none of the run's transfers is PENDING or PROCESSING.
func drain(ctx context.Context, st store, prefix string, timeout time.Duration) (check, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		n, err := st.runInFlight(ctx, prefix)
		if err != nil {
			return check{}, 0, err
		}
		waited := time.Since(start)
		if n == 0 {
			return check{name: "all transfers finished", passed: true, detail: "none left PENDING or PROCESSING"}, waited, nil
		}
		if time.Now().After(deadline) {
			return check{
				name:   "all transfers finished",
				detail: fmt.Sprintf("%d still PENDING or PROCESSING after %s", n, timeout),
			}, waited, nil
		}
		select {
		case <-ctx.Done():
			return check{}, 0, ctx.Err()
		case <-time.After(drainPoll):
		}
	}
}
