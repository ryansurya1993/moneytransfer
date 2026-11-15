# 9. An incremental ledger check that holds a merchant's payouts

In short: a background check verifies only new records, and pauses a merchant's payouts when
something doesn't add up.

## Context

A code change can break the money rules in ways the balance alone doesn't show: a double refund
through the normal path keeps balance and ledger in step, and a skipped refund leaves no entry at
all. A check that reads every row gets slower every year.

## Decision

The relay checks only what changed since its last round:

- ledger entries after a per-merchant checkpoint, read through the `(merchant_id,
  merchant_version)` index;
- transfers finished since the last round, found through a `ledger_checked_at` marker and a
  partial index.

Rules: versions have no gaps, each entry's `balance_after` is the previous one plus its amount, the
merchant row matches the newest entry, each debit is exactly its transfer's amount, a refund exists
exactly when a transfer failed, and every finished transfer has its debit. While there is work the
check keeps going; once caught up it waits 30 seconds.

A broken rule is stored as a finding and holds that merchant's payouts, in the same transaction:

- new transfers get `422 MERCHANT_PAYOUTS_HELD` and nothing is stored;
- accepted transfers wait as `PENDING`;
- transfers already at the provider finish, and refunds and top-ups still credit.

The alert `alert=ledger_mismatch` names the merchant, rule and ids, never amounts. A person
resolves each finding with a command that stores who and why; the last one lifts the hold.

## Considered options

- Full check every run: about 50 seconds at 10 million transfers on default Postgres settings,
  growing with the age of the data.
- Database triggers enforcing the rules at commit: about 3% slower on a busy merchant, and they
  only guard against writers the service doesn't allow.
- Alert only: a wrong balance could be spent until a person acts.
- Hold every merchant: one bad entry would stop all payouts.
- Hold an amount instead of payouts: needs a known amount at risk, and the balance itself is in
  doubt.

## Consequences

The cost of a round follows traffic, not history: on 10 million transfers and 100,000 merchants,
about 35 ms to find the merchants with new entries and 0.06 ms per such merchant. A false alarm
delays one merchant's payouts and never moves money. Checked entries are trusted afterwards, which
relies on only the service writing to its database.
