package loadtest

import "testing"

func TestGroupKeys(t *testing.T) {
	keys := groupKeys([]result{
		{key: "declined then accepted", status: 422},
		{key: "declined then accepted", status: 202},
		{key: "declined", status: 422},
		{key: "unavailable", status: 503},
		{key: "no answer", status: 0},
		{key: "unavailable then declined", status: 503},
		{key: "unavailable then declined", status: 422},
	})

	want := map[string]string{
		"declined then accepted":    "accepted",
		"declined":                  "declined",
		"unavailable":               "uncertain",
		"no answer":                 "uncertain",
		"unavailable then declined": "uncertain",
	}
	for key, group := range want {
		got := map[string]bool{"accepted": keys.accepted[key], "declined": keys.declined[key], "uncertain": keys.uncertain[key]}
		for g, in := range got {
			if in != (g == group) {
				t.Errorf("key %q: in %s = %v, want only in %s", key, g, in, group)
			}
		}
	}
}

func TestCheckLedger(t *testing.T) {
	start := snapshot{Balance: 1000, Version: 10}
	after := func(v int64) *int64 { return &v }
	tests := []struct {
		name   string
		end    snapshot
		window ledgerWindow
		want   bool
	}{
		{name: "every change has an entry", end: snapshot{Balance: 830, Version: 13}, window: ledgerWindow{Entries: 3, Sum: -170, LastBalanceAfter: after(830)}, want: true},
		{name: "no change", end: start, want: true},
		{name: "change without an entry", end: snapshot{Balance: 830, Version: 13}, window: ledgerWindow{Entries: 2, Sum: -170, LastBalanceAfter: after(830)}, want: false},
		{name: "entries miss part of the change", end: snapshot{Balance: 830, Version: 13}, window: ledgerWindow{Entries: 3, Sum: -70, LastBalanceAfter: after(830)}, want: false},
		{name: "newest balance_after is off", end: snapshot{Balance: 830, Version: 13}, window: ledgerWindow{Entries: 3, Sum: -170, LastBalanceAfter: after(800)}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkLedger(start, tt.end, tt.window)
			if c.passed != tt.want {
				t.Errorf("passed = %v, want %v (%s)", c.passed, tt.want, c.detail)
			}
		})
	}
}

func TestCheckTransferEntries(t *testing.T) {
	debited := func(status string) storedTransfer {
		return storedTransfer{Status: status, Amount: 100, Debits: 1, Debited: -100}
	}
	refunded := func(n int) storedTransfer {
		tr := debited("FAILED")
		tr.Refunds, tr.Refunded = n, int64(n)*100
		return tr
	}
	tests := []struct {
		name string
		t    storedTransfer
		want bool
	}{
		{name: "success debited", t: debited("SUCCESS"), want: true},
		{name: "unknown debited", t: debited("UNKNOWN"), want: true},
		{name: "failed refunded", t: refunded(1), want: true},
		{name: "debit missing", t: storedTransfer{Status: "SUCCESS", Amount: 100}, want: false},
		{name: "debited twice", t: storedTransfer{Status: "SUCCESS", Amount: 100, Debits: 2, Debited: -200}, want: false},
		{name: "debit of another amount", t: storedTransfer{Status: "SUCCESS", Amount: 100, Debits: 1, Debited: -90}, want: false},
		{name: "failed not refunded", t: refunded(0), want: false},
		{name: "failed refunded twice", t: refunded(2), want: false},
		{name: "success refunded", t: func() storedTransfer { tr := refunded(1); tr.Status = "SUCCESS"; return tr }(), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkTransferEntries([]storedTransfer{debited("SUCCESS"), tt.t})
			if c.passed != tt.want {
				t.Errorf("passed = %v, want %v (%s)", c.passed, tt.want, c.detail)
			}
		})
	}
}

func TestCheckProvider(t *testing.T) {
	tests := []struct {
		name   string
		status string
		payout string
		want   bool
	}{
		{name: "success paid", status: "SUCCESS", payout: "SUCCEEDED", want: true},
		{name: "failed declined", status: "FAILED", payout: "FAILED", want: true},
		{name: "unknown never recorded", status: "UNKNOWN", want: true},
		{name: "unknown paid", status: "UNKNOWN", payout: "SUCCEEDED", want: true},
		{name: "success never recorded", status: "SUCCESS", want: false},
		{name: "success declined", status: "SUCCESS", payout: "FAILED", want: false},
		{name: "failed but paid", status: "FAILED", payout: "SUCCEEDED", want: false},
		{name: "failed never recorded", status: "FAILED", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payouts := map[string]string{"other": "SUCCEEDED"}
			if tt.payout != "" {
				payouts["id-1"] = tt.payout
			}
			c := checkProvider([]storedTransfer{{ID: "id-1", Status: tt.status}}, payouts)
			if c.passed != tt.want {
				t.Errorf("passed = %v, want %v (%s)", c.passed, tt.want, c.detail)
			}
		})
	}
}

func TestCheckEventsPublished(t *testing.T) {
	tests := []struct {
		name      string
		events    int
		published int
		want      bool
	}{
		{name: "one published", events: 1, published: 1, want: true},
		{name: "no event", events: 0, published: 0, want: false},
		{name: "two events", events: 2, published: 2, want: false},
		{name: "not published", events: 1, published: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkEventsPublished([]storedTransfer{{Events: tt.events, PublishedEvents: tt.published}})
			if c.passed != tt.want {
				t.Errorf("passed = %v, want %v (%s)", c.passed, tt.want, c.detail)
			}
		})
	}
}

func TestCheckNotNegative(t *testing.T) {
	if !checkNotNegative(snapshot{Balance: 0}).passed {
		t.Error("balance 0 failed the check")
	}
	if checkNotNegative(snapshot{Balance: -1}).passed {
		t.Error("balance -1 passed the check")
	}
}

func TestCheckOneTransferPerKey(t *testing.T) {
	keys := keyOutcomes{
		accepted:  map[string]bool{"a": true, "b": true},
		declined:  map[string]bool{"d": true},
		uncertain: map[string]bool{"u": true},
	}
	tests := []struct {
		name   string
		stored []string
		want   bool
	}{
		{name: "one each", stored: []string{"a", "b"}, want: true},
		{name: "uncertain key stored", stored: []string{"a", "b", "u"}, want: true},
		{name: "accepted key missing", stored: []string{"a"}, want: false},
		{name: "accepted key stored twice", stored: []string{"a", "b", "b"}, want: false},
		{name: "declined key stored", stored: []string{"a", "b", "d"}, want: false},
		{name: "key never sent", stored: []string{"a", "b", "x"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts []storedTransfer
			for _, k := range tt.stored {
				ts = append(ts, storedTransfer{Key: k, Status: "SUCCESS", Amount: 1})
			}
			c := checkOneTransferPerKey(keys, ts)
			if c.passed != tt.want {
				t.Errorf("passed = %v, want %v (%s)", c.passed, tt.want, c.detail)
			}
		})
	}
}
