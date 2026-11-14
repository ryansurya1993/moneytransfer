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

// checkLedger proves every balance change during the run has a ledger entry, whoever caused it.
// Each change bumps the merchant version by one, so the versions between the snapshots must all be there.
func checkLedger(start, end snapshot, w ledgerWindow) check {
	change := end.Balance - start.Balance
	versions := end.Version - start.Version
	lastOK := w.Entries == 0 || (w.LastBalanceAfter != nil && *w.LastBalanceAfter == end.Balance)
	detail := fmt.Sprintf("balance changed by %d, %d entries for %d versions add up to %d", change, w.Entries, versions, w.Sum)
	if !lastOK {
		detail += fmt.Sprintf(", newest balance_after %d is not the balance", *w.LastBalanceAfter)
	}
	return check{
		name:   "ledger explains the balance",
		passed: w.Entries == versions && w.Sum == change && lastOK,
		detail: detail,
	}
}

// checkTransferEntries proves each transfer was debited once and refunded once only if it FAILED.
func checkTransferEntries(ts []storedTransfer) check {
	var badDebits, badRefunds int
	for _, t := range ts {
		if t.Debits != 1 || t.Debited != -t.Amount {
			badDebits++
		}
		refundOK := t.Refunds == 0
		if t.Status == "FAILED" {
			refundOK = t.Refunds == 1 && t.Refunded == t.Amount
		}
		if !refundOK {
			badRefunds++
		}
	}
	return check{
		name:   "each transfer debited once, refunded only if FAILED",
		passed: badDebits == 0 && badRefunds == 0,
		detail: fmt.Sprintf("%d transfers, %d with a wrong debit, %d with a wrong refund", len(ts), badDebits, badRefunds),
	}
}

// checkProvider compares each transfer's outcome with the payout the provider recorded under its id.
// A FAILED transfer was refunded, so the provider must have declined it, never paid it.
// UNKNOWN transfers are still being settled by the reconciler and only counted.
func checkProvider(ts []storedTransfer, payouts map[string]string) check {
	var successNotPaid, failedNotDeclined, unknown, unknownPaid int
	for _, t := range ts {
		got := payouts[t.ID]
		switch t.Status {
		case "SUCCESS":
			if got != "SUCCEEDED" {
				successNotPaid++
			}
		case "FAILED":
			if got != "FAILED" {
				failedNotDeclined++
			}
		case "UNKNOWN":
			unknown++
			if got == "SUCCEEDED" {
				unknownPaid++
			}
		}
	}
	return check{
		name:   "provider agrees with every outcome",
		passed: successNotPaid == 0 && failedNotDeclined == 0,
		detail: fmt.Sprintf("%d SUCCESS not paid, %d FAILED not declined; %d UNKNOWN (%d already paid)",
			successNotPaid, failedNotDeclined, unknown, unknownPaid),
	}
}

// checkEventsPublished proves each transfer got exactly one transfer.created event and the relay sent it.
func checkEventsPublished(ts []storedTransfer) check {
	var wrongCount, unpublished int
	for _, t := range ts {
		if t.Events != 1 {
			wrongCount++
		}
		if t.PublishedEvents != t.Events {
			unpublished++
		}
	}
	return check{
		name:   "every transfer's event published",
		passed: wrongCount == 0 && unpublished == 0,
		detail: fmt.Sprintf("%d transfers, %d without exactly one event, %d not published", len(ts), wrongCount, unpublished),
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
