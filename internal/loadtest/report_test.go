package loadtest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	var sorted []time.Duration
	for i := 1; i <= 100; i++ {
		sorted = append(sorted, time.Duration(i)*time.Millisecond)
	}
	tests := []struct {
		p    float64
		want time.Duration
	}{
		{p: 50, want: 50 * time.Millisecond},
		{p: 95, want: 95 * time.Millisecond},
		{p: 99, want: 99 * time.Millisecond},
		{p: 100, want: 100 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := percentile(sorted, tt.p); got != tt.want {
			t.Errorf("p%v = %v, want %v", tt.p, got, tt.want)
		}
	}
	if got := percentile(nil, 99); got != 0 {
		t.Errorf("p99 of nothing = %v, want 0", got)
	}
}

func TestResponseCounts(t *testing.T) {
	got := responseCounts([]result{
		{status: 202}, {status: 202, replayed: true}, {status: 422}, {status: 422}, {status: 503}, {status: 0},
	})
	want := []countLine{{"202 new", 1}, {"202 replay", 1}, {"422", 2}, {"503", 1}, {"no answer", 1}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestReportVerdict(t *testing.T) {
	pass := check{name: "ok", passed: true}
	fail := check{name: "broken"}
	tests := []struct {
		name     string
		report   Report
		wantLast string
		wantPass bool
	}{
		{name: "all pass", report: Report{checks: []check{pass, pass}}, wantLast: "PASS", wantPass: true},
		{name: "one fails", report: Report{checks: []check{pass, fail}}, wantLast: "FAIL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.report.Write(&buf)
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")

			if last := lines[len(lines)-1]; last != tt.wantLast {
				t.Errorf("last line %q, want %q", last, tt.wantLast)
			}
			if tt.report.Passed() != tt.wantPass {
				t.Errorf("Passed %v, want %v", tt.report.Passed(), tt.wantPass)
			}
		})
	}
}
