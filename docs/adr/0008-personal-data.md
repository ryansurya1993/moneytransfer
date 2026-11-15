# 8. Keep personal data out of logs, messages and responses

## Context

Recipient bank details are personal data under Indonesia's Personal Data Protection Law
(UU 27/2022), and account numbers count as specific (financial) personal data. Every copy outside
the database is another place it can leak from.

## Decision

- Account numbers are masked to the last four digits in logs and API responses.
- Request bodies and descriptions are never logged; error messages carry no personal data.
- Broker messages carry ids only; the worker loads the transfer from Postgres.
- Responses are an explicit allowlist of fields; the description, idempotency key and provider
  reference are never returned.
- Alerts carry ids and rule names, never amounts or balances.

The database keeps the full values: the payout needs them and they are the audit trail.

## Considered options

- Full account numbers in responses: simplest, but every response becomes another copy in client
  logs and screens.
- Full transfer details in messages: saves the worker one read, but copies personal data into the
  broker, its UI and the dead-letter queue.
- Amounts in alerts: faster to investigate, but balances end up in the log platform for its whole
  retention, readable by everyone with log access.

## Consequences

Logs can go to support staff and vendors; an investigation goes from an id to the database.
Encryption at rest, access auditing and a retention policy are still needed for production.
