package loadtest

import (
	"fmt"
	"math/rand/v2"
	"slices"

	"github.com/google/uuid"
)

// magicSuffixes are the account endings the mock provider answers with a fixed outcome.
var magicSuffixes = []string{"0001", "0002", "0003", "0004"}

type transferBody struct {
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	BankCode          string `json:"bank_code"`
	AccountNumber     string `json:"account_number"`
	AccountHolderName string `json:"account_holder_name"`
	ReferenceID       string `json:"reference_id"`
}

// request is one intended transfer. A duplicate sends the same request again.
type request struct {
	key  string
	body transferBody
}

// shot is what one tick of the clock sends.
type shot struct {
	req *request
	// retry resends an earlier request; twin sends the request twice at the same moment.
	retry bool
	twin  bool
}

type planOptions struct {
	runID            string
	ticks            int
	duplicatePercent int
	magicPercent     int
	minAmount        int64
	maxAmount        int64
}

// newPlan decides every tick up front. Half of the duplicates are retries of an earlier request,
// the other half are twins.
func newPlan(rng *rand.Rand, o planOptions) []shot {
	retryPercent := o.duplicatePercent / 2
	shots := make([]shot, 0, o.ticks)
	var sent []*request
	for range o.ticks {
		roll := rng.IntN(100)
		if roll < retryPercent && len(sent) > 0 {
			shots = append(shots, shot{req: sent[rng.IntN(len(sent))], retry: true})
			continue
		}
		req := &request{
			key: uuid.NewString(),
			body: transferBody{
				Amount:            o.minAmount + rng.Int64N(o.maxAmount-o.minAmount+1),
				Currency:          "IDR",
				BankCode:          "BCA",
				AccountNumber:     pickAccount(rng, o.magicPercent),
				AccountHolderName: "Load Test",
				ReferenceID:       runReference(o.runID),
			},
		}
		sent = append(sent, req)
		shots = append(shots, shot{req: req, twin: roll >= retryPercent && roll < o.duplicatePercent})
	}
	return shots
}

// pickAccount returns a 10-digit account number. Only the magic share may end in a magic suffix;
// any other account that happens to end in one is drawn again.
func pickAccount(rng *rand.Rand, magicPercent int) string {
	if rng.IntN(100) < magicPercent {
		return fmt.Sprintf("%06d%s", rng.IntN(1_000_000), magicSuffixes[rng.IntN(len(magicSuffixes))])
	}
	for {
		a := fmt.Sprintf("%010d", rng.Int64N(10_000_000_000))
		if !slices.Contains(magicSuffixes, a[6:]) {
			return a
		}
	}
}

// runReference tags every transfer of a run, so the checks can find them all, even those whose answer was lost.
func runReference(runID string) string {
	return "lt-" + runID
}
