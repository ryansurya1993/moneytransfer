package loadtest

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func testRand() *rand.Rand {
	return rand.New(rand.NewPCG(1, 2))
}

func TestNewPlan(t *testing.T) {
	const ticks = 10_000
	shots := newPlan(testRand(), planOptions{
		runID:            "run1",
		ticks:            ticks,
		duplicatePercent: 10,
		magicPercent:     5,
		minAmount:        500,
		maxAmount:        1500,
	})
	if len(shots) != ticks {
		t.Fatalf("got %d shots, want %d", len(shots), ticks)
	}

	var retries, twins, magic int
	seen := map[*request]bool{}
	refs := map[string]bool{}
	for i, s := range shots {
		if s.retry {
			retries++
			if !seen[s.req] {
				t.Fatalf("shot %d retries a request that was never sent", i)
			}
			continue
		}
		if seen[s.req] {
			t.Fatalf("shot %d sends an earlier request without being a retry", i)
		}
		seen[s.req] = true
		if s.twin {
			twins++
		}
		b := s.req.body
		if !strings.HasPrefix(b.ReferenceID, "lt-run1-") || refs[b.ReferenceID] {
			t.Errorf("shot %d: reference %q is not a new lt-run1- reference", i, b.ReferenceID)
		}
		refs[b.ReferenceID] = true
		if b.Amount < 500 || b.Amount > 1500 {
			t.Errorf("shot %d: amount %d outside 500-1500", i, b.Amount)
		}
		if slices.Contains(magicSuffixes, b.AccountNumber[6:]) {
			magic++
		}
	}

	// Each share is 5% of 10,000 ticks; allow for chance.
	for name, n := range map[string]int{"retries": retries, "twins": twins, "magic accounts": magic} {
		if n < 400 || n > 600 {
			t.Errorf("%s = %d, want about 500", name, n)
		}
	}
}

func TestPickAccountNeverMagicByChance(t *testing.T) {
	rng := testRand()
	// About 1 in 2,500 random accounts ends in a magic suffix, so 100,000 draws would hit dozens.
	for range 100_000 {
		a := pickAccount(rng, 0)
		if len(a) != 10 {
			t.Fatalf("account %q is not 10 digits", a)
		}
		if slices.Contains(magicSuffixes, a[6:]) {
			t.Fatalf("account %q ends in a magic suffix", a)
		}
	}
}
