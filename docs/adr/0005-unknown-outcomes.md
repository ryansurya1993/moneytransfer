# 5. A timeout is an unknown outcome, not a failure

## Context

A provider timeout doesn't say whether the money left. Refunding could pay the merchant back for a
transfer the bank paid; retrying with a new key could pay it twice.

## Decision

- The worker claims the transfer in a short transaction (`PENDING -> PROCESSING`), calls the
  provider with no transaction open, and records the outcome in a second transaction.
- The provider's idempotency key is the transfer id, so a retry, a redelivered message or a second
  worker gets the stored answer instead of a second payout.
- Timeouts, network errors, `5xx`, `429` and unknown codes are retried up to 3 attempts with
  exponential backoff and full jitter.
- Only a recognised failure code refunds, in the same transaction as the move to `FAILED`, guarded
  by `WHERE status = 'PROCESSING'`, so two workers can't both refund.
- Anything still unclear ends as `UNKNOWN`, with the money held.
- The relay's reconciler sends `UNKNOWN` transfers, and `PENDING`/`PROCESSING` ones whose message
  was lost, back to the worker: after 1 minute, then doubling up to an hour, 8 tries (about 3
  hours). After that it logs `alert=reconcile_exhausted` every 5 minutes until a person resolves
  them. The database clock decides what is due.

## Considered options

- Any `4xx` means failure: providers use `4xx` for non-failures, such as `409` for a key already
  processed, so a paid transfer could be refunded.
- Retries through RabbitMQ delay queues: frees worker slots during long waits, at the cost of more
  topology. Worth it when retries need minutes.
- A reconciler that calls a status endpoint: the right step with a real provider; the fake
  provider's answer to a repeated key is its stored result, which is what a status read returns.
- Recovery only by hand: no automatic repair after a short provider outage.

## Consequences

There is no refund on a guess. A merchant may see `UNKNOWN` for a while, and a person resolves what
the reconciler can't. The 3-hour window relies on the provider keeping idempotency keys for longer
(Stripe keeps them at least 24 hours).
