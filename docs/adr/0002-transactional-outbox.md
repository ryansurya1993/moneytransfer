# 2. Publish through a transactional outbox

In short: the transfer and its message are saved together, and a separate process sends the
message, so a stored transfer always reaches RabbitMQ.

## Context

A transfer must reach the broker if and only if it was stored and the balance deducted. Writing to
Postgres and to RabbitMQ separately loses one of the two when a process crashes in between.

## Decision

The api never talks to RabbitMQ. The submit transaction inserts an outbox row next to the transfer,
the balance change and the ledger entry. The relay then, in one transaction:

1. locks a batch of unsent rows (`WHERE published_at IS NULL ORDER BY id ... FOR UPDATE SKIP
   LOCKED`);
2. publishes each row with a publisher confirm and the `mandatory` flag, one at a time;
3. marks the confirmed rows as sent, stops at the first failure, and commits.

The next round starts at the row that failed. A message that no queue accepts comes back as
returned and counts as a failure: without `mandatory` it would be dropped and still confirmed.
Messages carry ids only (ADR 8).

## Considered options

- Publish from the api after the commit: a crash between the commit and the publish loses the
  message.
- Postgres as the queue (workers poll the transfers table): simpler, but the service is specified
  with a message broker, and polling load on the primary grows with the number of workers.
- Batched or asynchronous confirms: 20-30 times the throughput, but harder to reason about when
  something fails. RabbitMQ's documentation puts one confirm at a time at a few hundred messages per
  second, well above the load here.
- Roll back the whole batch on a failure: every confirmed message in it would be sent again.

## Consequences

Delivery is at least once (the broker can confirm and then the relay's commit can fail), so the
worker is idempotent. `SKIP LOCKED` lets several relays run without sending a row twice. Rows are
kept after sending; a partial index on unsent rows keeps the relay's query small as the table grows.
Retention is left for later.
