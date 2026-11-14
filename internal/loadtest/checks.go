package loadtest

import (
	"fmt"
	"net/http"
)

type check struct {
	name   string
	passed bool
	detail string
}

// keyOutcomes groups the keys the run sent by what the api answered.
type keyOutcomes struct {
	// accepted keys got at least one 202.
	accepted map[string]bool
	// declined keys got only 4xx answers, so the api stored nothing for them.
	declined map[string]bool
	// uncertain keys got no 202 but a 5xx or no answer at all: the transfer may or may not exist.
	uncertain map[string]bool
}

func groupKeys(results []result) keyOutcomes {
	k := keyOutcomes{accepted: map[string]bool{}, declined: map[string]bool{}, uncertain: map[string]bool{}}
	unsure := map[string]bool{}
	for _, r := range results {
		switch {
		case r.status == http.StatusAccepted:
			k.accepted[r.key] = true
		case r.status == 0 || r.status >= 500:
			unsure[r.key] = true
		}
	}
	for _, r := range results {
		switch {
		case k.accepted[r.key]:
		case unsure[r.key]:
			k.uncertain[r.key] = true
		default:
			k.declined[r.key] = true
		}
	}
	return k
}

func checkNotNegative(end snapshot) check {
	return check{
		name:   "balance is not negative",
		passed: end.Balance >= 0,
		detail: fmt.Sprintf("balance at the end %d", end.Balance),
	}
}

// checkBalanceDrop compares what the merchant lost with what its transfers still hold.
// A FAILED transfer was refunded; every other status keeps its amount.
func checkBalanceDrop(start, end snapshot, ts []storedTransfer) check {
	var held int64
	for _, t := range ts {
		if t.Status != "FAILED" {
			held += t.Amount
		}
	}
	drop := start.Balance - end.Balance
	return check{
		name:   "balance drop equals the amounts held by transfers",
		passed: drop == held,
		detail: fmt.Sprintf("balance dropped by %d, transfers not FAILED hold %d", drop, held),
	}
}

// checkOneTransferPerKey proves idempotency under load: every accepted key has exactly one transfer,
// and no declined key, or key the run never sent, has any.
func checkOneTransferPerKey(keys keyOutcomes, ts []storedTransfer) check {
	stored := map[string]int{}
	for _, t := range ts {
		stored[t.Key]++
	}
	var missing, extra, uncertainStored int
	for key := range keys.accepted {
		if stored[key] == 0 {
			missing++
		}
	}
	for key, n := range stored {
		switch {
		case keys.uncertain[key]:
			uncertainStored++
		case keys.accepted[key]:
			extra += n - 1
		default:
			extra += n
		}
	}
	return check{
		name:   "one transfer per accepted idempotency key",
		passed: missing == 0 && extra == 0,
		detail: fmt.Sprintf("%d accepted keys, %d transfers, %d missing, %d unexpected; %d uncertain keys (%d stored)",
			len(keys.accepted), len(ts), missing, extra, len(keys.uncertain), uncertainStored),
	}
}
