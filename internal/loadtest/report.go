package loadtest

import (
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"time"
)

// Report is the outcome of one run.
type Report struct {
	runID        string
	options      Options
	startBalance int64
	traffic      traffic
	drainTime    time.Duration
	transfers    []storedTransfer
	checks       []check
	// invalid explains why the checks can't be trusted, for example a transfer made outside the run.
	invalid string
}

// Passed reports whether every check passed.
func (r Report) Passed() bool {
	for _, c := range r.checks {
		if !c.passed {
			return false
		}
	}
	return true
}

// Valid reports whether the checks can be trusted. Something else moving the balance during the run
// makes them meaningless, whether they passed or not.
func (r Report) Valid() bool {
	return r.invalid == ""
}

// Write prints the report as plain text.
func (r Report) Write(w io.Writer) {
	o := r.options
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }

	p("Load test %s", r.runID)
	p("  target     %s, %d requests/s for %s, at most %d in flight", o.TargetURL, o.Rate, o.Duration, o.Concurrency)
	p("  balance    %d at the start, amounts %d-%d", r.startBalance, o.MinAmount, o.MaxAmount)
	p("")

	t := r.traffic
	p("Traffic")
	p("  requests   %d sent, %d skipped (no free slot), %.1f/s", len(t.results), t.skipped, float64(len(t.results))/t.elapsed.Seconds())
	lat := latencies(t.results)
	p("  latency    p50 %s, p95 %s, p99 %s", percentile(lat, 50), percentile(lat, 95), percentile(lat, 99))
	for _, line := range responseCounts(t.results) {
		p("  %-10s %d", line.label, line.count)
	}
	p("")

	p("Transfers created: %d (waited %s for their outcomes after sending)", len(r.transfers), r.drainTime.Round(time.Second))
	for _, s := range statusTotals(r.transfers) {
		if s.count > 0 {
			p("  %-10s %4d  amount %d", s.status, s.count, s.amount)
		}
	}
	p("")

	p("Checks")
	for _, c := range r.checks {
		verdict := "PASS"
		if !c.passed {
			verdict = "FAIL"
		}
		p("  %s  %s: %s", verdict, c.name, c.detail)
	}
	p("")

	switch {
	case r.invalid != "":
		p("INVALID: %s", r.invalid)
	case r.Passed():
		p("PASS")
	default:
		p("FAIL")
	}
}

func latencies(results []result) []time.Duration {
	l := make([]time.Duration, len(results))
	for i, r := range results {
		l[i] = r.latency
	}
	slices.Sort(l)
	return l
}

// percentile uses the nearest-rank method on sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1].Round(time.Millisecond)
}

type countLine struct {
	label string
	count int
}

// responseCounts counts answers by status code, with replays split from new 202s.
func responseCounts(results []result) []countLine {
	byCode := map[int]int{}
	var replayed, unanswered int
	for _, r := range results {
		switch {
		case r.status == 0:
			unanswered++
		case r.status == http.StatusAccepted && r.replayed:
			replayed++
		default:
			byCode[r.status]++
		}
	}
	lines := []countLine{
		{"202 new", byCode[http.StatusAccepted]},
		{"202 replay", replayed},
	}
	delete(byCode, http.StatusAccepted)
	for _, code := range slices.Sorted(maps.Keys(byCode)) {
		lines = append(lines, countLine{fmt.Sprint(code), byCode[code]})
	}
	return append(lines, countLine{"no answer", unanswered})
}

type statusTotal struct {
	status string
	count  int
	amount int64
}

func statusTotals(ts []storedTransfer) []statusTotal {
	order := []string{"SUCCESS", "FAILED", "UNKNOWN", "PROCESSING", "PENDING"}
	totals := make([]statusTotal, len(order))
	for i, s := range order {
		totals[i].status = s
	}
	for _, t := range ts {
		if i := slices.Index(order, t.Status); i >= 0 {
			totals[i].count++
			totals[i].amount += t.Amount
		}
	}
	return totals
}
