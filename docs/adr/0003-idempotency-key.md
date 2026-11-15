# 3. The idempotency key identifies the intended transfer

## Context

Clients retry when an answer is lost. A retry must never create a second transfer, and a declined
request should be allowed to succeed later, for example after a top-up.

## Decision

`Idempotency-Key` is required on `POST /transfer`. It is stored on the transfer with a hash of the
request, under `UNIQUE (merchant_id, idempotency_key)`, and claimed by the first insert of the
submit transaction.

- Same key, same body: the original transfer comes back with `202` and `Idempotent-Replayed: true`,
  showing its current status.
- Same key, different body: `422 IDEMPOTENCY_KEY_REUSED`.
- A retry that arrives while the original is still committing waits a few milliseconds on the
  unique index and then gets the original.
- Declines, validation errors and server errors store nothing, so the same key is evaluated again.

A replay answers `202` like the first request, so a client that checks for exactly `202` doesn't
mistake a replay for a failure and resubmit with a new key.

## Considered options

- A separate table caching full responses, with expiry: exact replays and keys that expire, more
  code than one endpoint needs.
- Remembering declines, so the same key always gets the same `422`: needs an extra status and a
  second transaction, and protects only against clients that retry a `4xx`.
- A cache in front (Redis): a second store for what the unique constraint already does.
- An optional key: requests without one can create duplicates.
- `200` for a replay: clients checking for `202` treat it as a failure and resubmit with a new key.

## Consequences

The database, not application code, decides what is a duplicate, across every api replica. Keys
never expire.
