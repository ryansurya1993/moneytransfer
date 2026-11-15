# 1. Deduct the balance with one conditional UPDATE

In short: checking the balance and taking the money are one database statement, so two requests
can't both pass the check.

## Context

Every transfer of a merchant deducts from one balance row. Two concurrent requests must never
overdraw it, and under load every request for that merchant competes for the same row.

## Decision

The submit transaction deducts with one statement, under `READ COMMITTED`:

```sql
UPDATE merchants
SET balance = balance - $1, version = version + 1
WHERE id = $2 AND balance >= $1 AND payouts_held_reason IS NULL
RETURNING balance, version
```

Postgres locks the row, so concurrent deductions for one merchant queue up. Zero updated rows means
insufficient balance (or a payout hold, see ADR 9). `CHECK (balance >= 0)` backs it up. The UPDATE
is the last write before the ledger entry, after the transfer and outbox inserts, so the hot row is
locked for as short a time as possible.

## Considered options

- `SELECT ... FOR UPDATE`, check in Go, then UPDATE: holds the lock two to three times longer.
  Worth it when a deduction needs rules in Go (daily limits, fees) or touches several accounts.
- Optimistic locking with retries: on one busy row most attempts conflict, which becomes a retry
  storm.
- `SERIALIZABLE`: correct, but every transaction can fail with a serialization error and needs a
  retry, for no extra safety on a single-row check.
- UPDATE first in the transaction: declines fail on the first statement, but every accepted
  transfer holds the lock through all the inserts.

## Consequences

One statement is both the check and the deduction; there is no read-then-write to race on.
Throughput per merchant is about one deduction per lock hold. A very busy merchant would need its
balance split over several rows, or batched deductions.
