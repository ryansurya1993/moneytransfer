# 4. A ledger entry and a version for every balance change

In short: every balance change is written down with a counter, so the balance can be explained and
a missing record shows up.

## Context

"Why is the balance X?" must be answerable from stored data, and a missing record must be
detectable, not only absent.

## Decision

The merchant row keeps the balance and a version counter, raised by one in the same UPDATE that
changes the balance. Every change appends a `balance_entries` row (`OPENING`, `DEPOSIT`, `DEBIT` or
`REFUND`) with `balance_after` and `merchant_version`, written in the same transaction while the
row is locked. The schema enforces:

- `UNIQUE (merchant_id, merchant_version)`: one entry per version;
- `UNIQUE (transfer_id, type)`: at most one debit and one refund per transfer;
- the sign of the amount by type, and `balance_after >= 0`.

The version is a counter only; it is never checked in a `WHERE` clause (this is not optimistic
locking).

## Considered options

- Balance column only: least code, but no history; a refund leaves no record of its own.
- Balance derived from entries only: one source of truth, but each deduction then needs an explicit
  lock and a sum or a periodic snapshot.
- Entry written before the UPDATE: entries could be ordered differently from the real balance
  changes, and a running balance could show values that never existed.
- Ids instead of a version: rolled-back transactions leave gaps in ids, so a gap says nothing.

## Consequences

The balance can be rebuilt from its entries, and a missing entry shows as a gap in the versions.
The ledger check (ADR 9) builds on both.
