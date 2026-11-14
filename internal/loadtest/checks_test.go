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

func TestCheckBalanceDrop(t *testing.T) {
	start := snapshot{Balance: 1000}
	ts := []storedTransfer{
		{Status: "SUCCESS", Amount: 100},
		{Status: "UNKNOWN", Amount: 50},
		{Status: "PROCESSING", Amount: 20},
		{Status: "FAILED", Amount: 300},
	}
	tests := []struct {
		name       string
		endBalance int64
		want       bool
	}{
		{name: "failed transfer refunded", endBalance: 830, want: true},
		{name: "refund missing", endBalance: 530, want: false},
		{name: "refunded twice", endBalance: 1130, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkBalanceDrop(start, snapshot{Balance: tt.endBalance}, ts)
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
