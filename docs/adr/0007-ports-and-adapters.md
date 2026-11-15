# 7. Ports and adapters in an idiomatic Go layout

In short: the money rules live in one package that knows nothing about the database, so they can
be tested on their own.

## Context

The money rules must be testable without a database, and each library should be replaceable
without touching those rules.

## Decision

- `internal/transfer` holds the domain types, the flows (submit, payout, reconcile, ledger check)
  and the interfaces they need (ports).
- `internal/adapter/...` implements them with Postgres, RabbitMQ and HTTP; `cmd/<binary>` only
  wires them together.
- A unit of work runs a callback with repositories bound to one transaction, so a flow can span
  several tables without the core knowing about transactions.
- Each external library lives in one package. The linter (depguard) stops the core from importing
  any database, broker or HTTP library.

## Considered options

- Transaction in the context: shorter, but implicit; using the outer context inside the callback
  silently runs a query outside the transaction.
- `*sqlx.Tx` passed through the core: the core would import the driver.
- One repository method per operation: the flow and its step order move into SQL, where fakes
  can't test them.
- Separate domain and application packages: more packages to move between for one business area.

## Consequences

Core tests use in-memory fakes; integration tests run the adapters against real Postgres and
RabbitMQ.
