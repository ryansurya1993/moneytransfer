# 6. Crash on broker loss, wait on database loss, sort messages by cause

## Context

Broker connections drop, the database restarts, and some messages can never succeed. Each needs a
different answer, because RabbitMQ counts some outcomes against a message's delivery limit and not
others: a reject and a lost connection count, a nack doesn't.

## Decision

- **Broker connection lost:** the relay and the worker exit non-zero and Docker restarts them.
  The normal startup path reconnects and declares the topology again.
- **Database unreachable:** the worker keeps the message and retries the database step with
  backoff until it works or the process stops. Crashing would count a delivery against every
  message in flight.
- **Malformed message or unknown transfer:** rejected without requeue, straight to the dead-letter
  queue `transfers.dlq`.
- **Unexpected error or panic:** rejected with requeue; after 5 failed deliveries
  (`x-delivery-limit` on a quorum queue) it moves to the dead-letter queue.
- **Shutdown:** unfinished messages are nacked, which returns them without counting.

Messages are acknowledged only after the database commit.

## Considered options

- Reconnect inside the process with backoff: no restarts, but about 100 more lines with tricky
  cases (publishes in flight, consumers, topology).
- Crash on every error: a database outage would push good transfers into the dead-letter queue.
- Nack every failure: a poison message would loop forever.

## Consequences

Restarts lose nothing: unsent events wait in the outbox, unacknowledged messages are redelivered,
and processing is idempotent. A long broker outage means repeated restarts, which Docker spaces
out. The dead-letter queue holds only ids.
