# moneytransfer

A payout service in Go. A merchant submits a bank transfer with `POST /transfer`; the merchant
balance is deducted right away, and the payout happens in the background through RabbitMQ and a
payment provider. A failed payout is refunded. Postgres keeps balances, transfers and a ledger
of every balance change.

What it guarantees:

- Two requests at the same moment can never spend more than the balance.
- Sending the same request again (same `Idempotency-Key`) never creates a second transfer.
- A transfer is never paid out twice, even when the bank answers late or a worker crashes.
- A failed transfer is refunded once, and only when the bank says it failed.
- The balance always matches its history: the sum of its ledger entries.

Stack: Go 1.26, PostgreSQL 18, RabbitMQ 4.3, HAProxy 3.4, Docker Compose.

## Contents

- [Important notes](#important-notes)
  - [Data protection](#data-protection)
  - [Other notes](#other-notes)
- [Quick start](#quick-start)
- [Architecture](#architecture)
  - [Why it is built this way](#why-it-is-built-this-way)
- [Where to look in the code](#where-to-look-in-the-code)
- [Guide](#guide)
  - [What runs](#what-runs)
  - [Try it by hand](#try-it-by-hand)
  - [Simulated provider errors](#simulated-provider-errors)
  - [Burst test](#burst-test)
  - [Check it yourself](#check-it-yourself)
  - [Run the tests](#run-the-tests)
  - [Top up or reset](#top-up-or-reset)
- [API](#api)
- [Assumptions](#assumptions)
- [Out of scope](#out-of-scope)
- [Future improvements](#future-improvements)

## Important notes

### Data protection

Recipient bank details are personal data under Indonesia's Personal Data Protection Law
(UU 27/2022), and account numbers count as specific (financial) personal data. The service:

- masks account numbers everywhere except the database: logs and API responses show `****6789`;
- never logs request bodies or the transfer description, and never puts personal data in error
  messages;
- sends only ids through RabbitMQ (`transfer_id`, `correlation_id`), so the broker and its UI
  hold no personal data;
- keeps the full values only in Postgres, which the payout needs and which serves as the audit
  trail. Logs reach a transfer through its `transfer_id` or `correlation_id`.

Not done here, needed in production: encryption of account numbers at rest, a retention and
deletion policy, database access auditing, and a breach-notification process (the law gives
3 x 24 hours).

### Other notes

- Amounts are integers in whole rupiah (IDR only). There are no fractions anywhere.
- Passwords in `docker-compose.yml` and `.env.example` are local development values.
- There is no authentication: one hardcoded merchant (`merchant_id = 1`).

## Quick start

Requirements:

- Docker Engine 25+ with Compose v2.20+ (Docker Desktop works).
- `make` is optional: every command has a Docker-only version.
- Go 1.26, only to run the tests outside Docker.
- The commands are for bash. On Windows use Git Bash or WSL (PowerShell's `curl` is a different
  command).

| What | With make | With Docker only |
|---|---|---|
| Start everything | `make up` | `docker compose up -d --build --wait` |
| Burst test with 3 workers | `make loadtest WORKERS=3` | `docker compose up -d --build --wait --scale worker=3`<br>`docker compose run --rm --build --no-deps loadtest` |
| Add money to the merchant | `make topup AMOUNT=1000000000` | `docker compose run --rm --build --no-deps loadtest topup 1000000000` |
| Stop, keep the data | `make down` | `docker compose down` |
| Stop and delete the data | | `docker compose down -v` |

Starting builds one image with all binaries, starts Postgres, RabbitMQ and the services, runs the
migrations, and seeds merchant 1 with 1,000,000,000 rupiah. It returns once everything is healthy.

Then send a transfer:

```bash
curl -i -X POST localhost:18080/transfer \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-1' \
  -d '{"amount":150000,"currency":"IDR","bank_code":"BCA","account_number":"1234566789","account_holder_name":"Budi Santoso","reference_id":"INV-001"}'
```

```
HTTP/1.1 202 Accepted
location: /transfer/01a1d891-e9f2-7ee6-9809-e740d06dba54
x-correlation-id: 69306aea-ab47-406d-a3bd-61fa8c0257c6

{"id":"01a1d891-e9f2-7ee6-9809-e740d06dba54","status":"PENDING","amount":150000,"currency":"IDR",
 "bank_code":"BCA","account_number":"****6789","account_holder_name":"Budi Santoso",
 "reference_id":"INV-001","failure_code":null,"created_at":"...","updated_at":"..."}
```

The api answers in a few milliseconds, with the balance already deducted. The payout runs in the
background; the fake bank takes 0.2-2 seconds to answer (`MOCK_MIN_DELAY` and `MOCK_MAX_DELAY`
in `.env`), so check again a moment later:

```bash
curl localhost:18080/transfer/01a1d891-e9f2-7ee6-9809-e740d06dba54
```

```
{"id":"01a1d891-...","status":"SUCCESS",...}
```

The [Guide](#guide) below shows what else to try, and the [API](#api) section lists the request
rules and answers.

## Architecture

```mermaid
flowchart LR
    merchant([Merchant]) -->|POST /transfer| haproxy
    haproxy -->|round robin| api["api x2"]
    api -->|"one transaction: transfer, outbox event, balance, ledger"| pg[(PostgreSQL)]
    relay -->|reads unsent outbox events| pg
    relay -->|publish, wait for confirm| mq[[RabbitMQ: transfers]]
    mq -->|deliver| worker
    mq -.->|over the delivery limit| dlq[[transfers.dlq]]
    worker -->|"POST /payouts, Idempotency-Key = transfer id"| provider[mockprovider]
    worker -->|"claim, then record outcome and refund"| pg
    relay -.->|"every 30s: re-queue transfers left without an outcome"| pg
    relay -.->|"check new ledger entries; hold a merchant's payouts on a mismatch"| pg
```

A transfer moves through these statuses:

```
PENDING -> PROCESSING -> SUCCESS
                      -> FAILED   (refunded in the same transaction)
                      -> UNKNOWN  (no definite answer from the provider; money stays held)

UNKNOWN, or PENDING/PROCESSING whose message was lost -> reconciler -> PROCESSING again
```

| Package | Role |
|---|---|
| `internal/transfer` | Core: domain types, validation, submit, payout, reconcile and ledger-check flows, and the interfaces they need. Imports no database, broker or HTTP library (enforced by the linter). |
| `internal/adapter/httpapi` | HTTP handlers, RFC 9457 errors, correlation id and access log middleware |
| `internal/adapter/postgres` | Unit of work, repositories, outbox store |
| `internal/adapter/rabbitmq` | Publisher with confirms, consumer, queue topology |
| `internal/adapter/provider` | HTTP client for the payout provider |
| `internal/outbox` | Relay loop |
| `internal/mockprovider` | The fake bank |
| `internal/loadtest` | Burst test and checks |
| `internal/pgtest` | Postgres container for integration tests; never in a binary (enforced by the linter) |
| `internal/config`, `internal/logging`, `internal/health` | Env config, correlation id in logs, health endpoints |
| `cmd/*` | One `main` per binary, wiring only |
| `migrations/` | Schema (golang-migrate) |

### Why it is built this way

Checking the balance and taking the money happen in one step, so two requests can't both pass the
check. The api deducts the balance with one conditional UPDATE
(`... WHERE id = $2 AND balance >= $1`): Postgres locks the merchant row, so concurrent requests
queue up, and zero updated rows means insufficient balance. The same transaction stores the
transfer, an outbox event and a ledger entry, so there is never a deduction without its transfer
or a transfer without its message. The relay publishes the event afterwards and marks it sent only
once RabbitMQ has confirmed it.

A transfer is never paid twice, and money is refunded only when the bank says no. The worker calls
the provider with no transaction open and the transfer id as the provider's idempotency key, so a
retry or a second worker gets the stored answer instead of a second payout. Only a failure code the
service knows leads to a refund, in the same transaction as the move to `FAILED`. A timeout or an
unknown answer leaves the transfer `UNKNOWN` with the money held, and the relay's reconciler asks
again later: refunding a transfer the bank paid would pay it twice.

Every balance change leaves a record, and a background check makes sure the records add up. Each
change writes a ledger entry with the merchant's version number. The relay checks new entries and
finished transfers against the money rules, reading only what changed since its last round. A
broken rule holds that merchant's payouts until a person resolves it; a false alarm delays payouts
and never moves money.

| Decision | Why | Record |
|---|---|---|
| One conditional UPDATE for the balance | No other request can come between the check and the deduction | [ADR 1](docs/adr/0001-conditional-update-for-the-balance.md) |
| Transactional outbox; the relay marks only confirmed events | A stored transfer always reaches the broker | [ADR 2](docs/adr/0002-transactional-outbox.md) |
| Idempotency enforced by a unique constraint | A lost answer never becomes a second transfer; a decline can be retried | [ADR 3](docs/adr/0003-idempotency-key.md) |
| A ledger entry and a version for every balance change | The balance is explained by its history; a missing entry shows as a gap | [ADR 4](docs/adr/0004-ledger-and-version.md) |
| A timeout is unknown; refund only on a known failure | No refund on a guess; the reconciler finds the answer later | [ADR 5](docs/adr/0005-unknown-outcomes.md) |
| Crash on broker loss, wait on database loss | Restarts lose nothing, and an outage doesn't push good messages to the dead-letter queue | [ADR 6](docs/adr/0006-failure-handling-in-the-worker.md) |
| Ports and adapters | The money rules are tested without a database | [ADR 7](docs/adr/0007-ports-and-adapters.md) |
| Ids only in messages; masked logs and responses | Personal data stays in the database | [ADR 8](docs/adr/0008-personal-data.md) |
| Incremental ledger check with a payout hold | Money bugs are found within a minute, at a cost that follows traffic, not history | [ADR 9](docs/adr/0009-incremental-ledger-check.md) |

Two api replicas run behind HAProxy, so the balance guard holds across processes, not only inside
one. HAProxy checks each replica every second and sends requests only to healthy ones; it retries a
request only when it couldn't connect, so a `POST` is never sent twice.

HAProxy waits at most 10 seconds for a replica, the same as the api's own write timeout
(`HTTP_WRITE_TIMEOUT`); after that the api can't answer anyway. When changing one, change the other
(`deploy/haproxy/haproxy.cfg`). They can't share one setting because HAProxy doesn't understand Go
durations like `1m30s`.

## Where to look in the code

| What | Where |
|---|---|
| The balance deduction | [`internal/adapter/postgres/balance.go`](internal/adapter/postgres/balance.go) (`Debit`) |
| Submit: what is written, in which order | [`internal/transfer/service.go`](internal/transfer/service.go) |
| Payout, retries and refund | [`internal/transfer/process.go`](internal/transfer/process.go) |
| Re-checking transfers without an answer | [`internal/transfer/reconcile.go`](internal/transfer/reconcile.go) |
| Ledger check and payout hold | [`internal/transfer/ledger.go`](internal/transfer/ledger.go) |
| Sending events to RabbitMQ | [`internal/outbox/relay.go`](internal/outbox/relay.go), [`internal/adapter/postgres/outbox.go`](internal/adapter/postgres/outbox.go) |
| Money under concurrency | `TestConcurrentSubmitsNeverOverdraw` and `TestConcurrentDuplicatesCreateOneTransfer` in [`store_test.go`](internal/adapter/postgres/store_test.go), `TestConcurrentWorkersRefundOnce` in [`process_test.go`](internal/adapter/postgres/process_test.go) |
| The burst test's checks | [`internal/loadtest/checks.go`](internal/loadtest/checks.go) |

## Guide

### What runs

| Service | What it does | Host port |
|---|---|---|
| `haproxy` | Spreads requests over the api replicas | `18080` |
| `api` (2 replicas) | HTTP API: validates, deducts the balance, stores the transfer | - |
| `relay` | Publishes stored events to RabbitMQ, re-queues stuck transfers, checks the ledger | - |
| `worker` | Calls the provider, records the outcome, refunds failures | - |
| `mockprovider` | Fake bank with random delays and outcomes | - |
| `postgres`, `rabbitmq` | Storage and broker (RabbitMQ UI: user and password `moneytransfer`) | `18672` (UI) |
| `migrate`, `seed` | One-shot jobs that create the schema and the merchant | - |

Both ports listen on 127.0.0.1 only, so the unauthenticated API and the RabbitMQ UI are not
reachable from the network. Ports and every other setting can be changed in a `.env` file (copy
`.env.example`, which lists all variables with their defaults).

### Try it by hand

After the first transfer from the [Quick start](#quick-start):

- Send the same POST again: the answer is the same transfer with `Idempotent-Replayed: true`, and
  nothing is deducted twice.
- Change the body but keep the key: the answer is `422 IDEMPOTENCY_KEY_REUSED`. Use a new key for
  a new transfer.
- Send a few more: about 1 in 10 ends `FAILED` and a few end `UNKNOWN`. That is on purpose: see
  [Simulated provider errors](#simulated-provider-errors).

### Simulated provider errors

The fake provider fails on purpose, so the failure paths can be seen, in two ways.

**On demand.** Account numbers ending in these digits always give the same outcome:

| Ends in | Provider does | Transfer ends as |
|---|---|---|
| `0001` | Rejects with `INVALID_ACCOUNT` | `FAILED` (`failure_code: INVALID_ACCOUNT`), balance refunded |
| `0002` | Pays, but the answer arrives after the worker gave up | `SUCCESS` after a retry with the same key, paid once |
| `0003` | Answers 503 every time | `UNKNOWN` after 3 attempts, money held (see below) |
| `0004` | Rejects with a code the service doesn't know | `UNKNOWN`, money held, never refunded on a guess (see below) |

**At random,** for any other account number:

- about 10% are rejected with a bank error such as `REJECTED_BY_BANK`;
- about 5% are paid, but the answer comes too late; the worker retries and gets `SUCCESS`;
- about 5% get a 503; the worker retries, and most of them end `SUCCESS`.

The three percentages are settings of the fake provider: `MOCK_FAILURE_PERCENT` (default 10),
`MOCK_TIMEOUT_PERCENT` (5) and `MOCK_UNAVAILABLE_PERCENT` (5). Setting all three to 0 turns the
random errors off, for example for a clean demo. Only the fake provider restarts; the data stays.

*Option 1, for one run (bash).* Put the three settings in front of the start command:

```bash
MOCK_FAILURE_PERCENT=0 MOCK_TIMEOUT_PERCENT=0 MOCK_UNAVAILABLE_PERCENT=0 make up
```

With Docker only, put the same three settings in front of `docker compose up -d --build --wait`.
To turn the random errors back on, run the start command again without them.

*Option 2, until changed back (any terminal).* Copy the example settings file to `.env`:

```bash
cp .env.example .env
```

Open `.env` in an editor, change these three lines to 0, save, and run the start command:

```
MOCK_FAILURE_PERCENT=0
MOCK_TIMEOUT_PERCENT=0
MOCK_UNAVAILABLE_PERCENT=0
```

To turn them back on, set the lines back to 10, 5 and 5 (or delete `.env`), and run the start
command again. `.env` is local only; git ignores it.

**What happens after an error.** The worker retries timeouts and 503s by itself, up to 3 attempts,
always with the same provider key, so a retry never pays twice. Then:

- `FAILED` is final, and the amount is already back on the balance. To try again, send a new
  request with a new `Idempotency-Key`; the old key returns the failed transfer.
- `UNKNOWN` means the provider gave no clear answer, so the bank may have paid. The money stays
  held. Don't send it again: that could pay twice.
  - The service asks the provider again by itself, with the same key: after 1 minute, then 2, 4,
    8 minutes and so on, at most an hour apart. Meanwhile the status may briefly show
    `PROCESSING`, then `SUCCESS` or `FAILED`.
  - After 8 checks (about 3 hours) without an answer, the relay logs `alert=reconcile_exhausted`
    every 5 minutes until a person resolves it.
  - Settings: `RECONCILE_AFTER` (first wait) and `RECONCILE_MAX_ATTEMPTS` (number of checks).

To watch this without waiting, start the stack with a short gap and fewer checks, and send a
transfer to an account ending in `0003`; within about 5 minutes `docker compose logs relay` shows
the alert:

```bash
RECONCILE_AFTER=10s RECONCILE_MAX_ATTEMPTS=3 make up
```

With Docker only, put the same two settings in front of `docker compose up -d --build --wait`.

### Burst test

Start it with the burst test command from the [Quick start](#quick-start) table.

> **Keep `--no-deps`.** `docker compose run` without it resets the dependencies to the worker
> count in `docker-compose.yml`, which silently stops workers 2 and 3 in the middle of a scaled
> setup.

What it sends, for 30 seconds:

- 20 requests per second on a fixed schedule, even when the api is slow (at most 20 waiting at
  once), so a slowdown shows up in the latency numbers instead of being hidden.
- Random amounts between 50,000 and 5,000,000. The run spends a bit more than the seeded balance,
  so it ends with `422 INSUFFICIENT_BALANCE` answers.
- About 10% duplicate keys: half are later retries, half are sent twice at the same moment.
- About 5% accounts ending in `0001`-`0004`.

The requests go through HAProxy, so both api replicas deduct from the same balance at the same time.

Then it waits until none of its transfers is `PENDING` or `PROCESSING` and checks Postgres
directly (the API has no balance endpoint, on purpose) and the mock provider:

1. Every transfer finished.
2. The balance is not negative.
3. Every balance change during the run has its ledger entry, and the entries add up to the change.
4. Each transfer has one debit, and one refund only if it `FAILED`.
5. Each accepted idempotency key has exactly one transfer.
6. The provider agrees: every `SUCCESS` was paid, every `FAILED` (refunded) one was declined.
7. Each transfer has exactly one event in the outbox, and it was published.

The test can run while other things happen on the merchant (a manual transfer, a top-up, a
refund): the checks tell those apart through the ledger. It's meant for test databases: it finds
its transfers by a reference that has no index. If the merchant's payouts are on hold, it stops
before sending anything (exit code 2).

A run on a laptop with 3 workers:

```
Load test 20261114T112415Z-2d09
  target     http://haproxy:8080, 20 requests/s for 30s, at most 20 in flight
  balance    1000000000 at the start, amounts 50000-5000000

Traffic
  requests   629 sent, 0 skipped (no free slot), 21.0/s
  latency    p50 6ms, p95 13ms, p99 19ms
  202 new    423
  202 replay 47
  422        159
  no answer  0

Transfers created: 423 (waited 0s for their outcomes after sending)
  SUCCESS     365  amount 968074442
  FAILED       46  amount 120380428
  UNKNOWN      12  amount 31918875
  other balance changes during the run: 0

Checks
  PASS  all transfers finished: none left PENDING or PROCESSING
  PASS  balance is not negative: balance at the end 6683
  PASS  ledger explains the balance: balance changed by -999993317, 469 entries for 469 versions add up to -999993317
  PASS  each transfer debited once, refunded only if FAILED: 423 transfers, 0 with a wrong debit, 0 with a wrong refund
  PASS  one transfer per accepted idempotency key: 423 accepted keys, 423 transfers, 0 missing, 0 unexpected; 0 uncertain keys (0 stored)
  PASS  provider agrees with every outcome: 0 SUCCESS not paid, 0 FAILED not declined; 12 UNKNOWN (0 already paid)
  PASS  every transfer's event published: 423 transfers, 0 without exactly one event, 0 not published

PASS
```

Exit code 0 means every check passed, 1 means a check failed, 2 means the run couldn't be judged
(setup problem, or the balance is too low). A second run needs more money: see
[Top up or reset](#top-up-or-reset).

### Check it yourself

The balance and its ledger (the two numbers are always equal):

```bash
docker compose exec postgres psql -U moneytransfer -c \
  "SELECT balance, (SELECT sum(amount) FROM balance_entries WHERE merchant_id = 1) AS ledger_sum FROM merchants WHERE id = 1;"
```

Transfers by status:

```bash
docker compose exec postgres psql -U moneytransfer -c \
  "SELECT status, count(*), sum(amount) FROM transfers GROUP BY status ORDER BY status;"
```

A failed transfer's ledger entries, a debit and its refund:

```bash
docker compose exec postgres psql -U moneytransfer -c \
  "SELECT type, amount, balance_after FROM balance_entries WHERE transfer_id = '<id>' ORDER BY id;"
```

One request through every service, by the correlation id from the response header (the HAProxy
line also names the api replica that answered):

```bash
docker compose logs haproxy api worker | grep 69306aea-ab47-406d-a3bd-61fa8c0257c6
```

The RabbitMQ UI at http://localhost:18672 shows the `transfers` queue and the `transfers.dlq`
dead-letter queue.

The ledger check, how to see it hold a merchant's payouts, and what to do when its alert fires:
[docs/ledger-check.md](docs/ledger-check.md).

### Run the tests

| Command | What runs | Needs |
|---|---|---|
| `make test` (`go test ./...`) | Unit tests | Go 1.26 |
| `make test-integration` (`go test -tags=integration -p 1 ./...`) | Unit + integration tests against real Postgres and RabbitMQ (started automatically in Docker) | Go 1.26, Docker |
| `make test-race` | Unit tests with the race detector | Docker |
| `make lint` | golangci-lint | Docker |

Without make, the race and lint checks run on every push through GitHub Actions (see below).

The integration tests cover the parts where money can go wrong: concurrent deductions on one
balance, concurrent duplicate keys, rollback when a step fails or panics, commit errors, a refund
that two workers race for, relays that compete for the same rows, and a message that always fails,
which stops after 6 tries (the first plus 5 retries) and is parked in the dead-letter queue. The
burst test's own checks are tested too: each must fail on deliberately damaged data.

GitHub Actions runs the same lint and every test, integration included, with the race detector
after each push to `main` and on each pull request (`.github/workflows/ci.yml`). It is a check
after the push, not a gate: nothing blocks a push to `main` on it.

### Top up or reset

Use the commands from the [Quick start](#quick-start) table. A top-up goes through the same code
as a refund and writes a `DEPOSIT` ledger entry, so the ledger still explains the balance. It has
no idempotency key: running it twice deposits twice.

Stopping keeps the data. Stopping with `-v` also deletes it, and the next start begins again from
the seeded balance.

## API

`POST /transfer` submits a transfer, `GET /transfer/{id}` reads it. Request rules:

| Field | Rule |
|---|---|
| `Idempotency-Key` header | Required, 1-255 characters |
| `amount` | JSON integer, 1 to 250,000,000 |
| `currency` | `IDR` |
| `bank_code` | 2-20 letters or digits |
| `account_number` | 5-20 digits (kept as text, leading zeros matter) |
| `account_holder_name` | 1-100 characters |
| `reference_id`, `description` | Optional, up to 64 and 255 characters |

Unknown fields are rejected, and all field errors come back at once. Errors come back as JSON in a
standard format (RFC 9457), with a `code` that never changes:

```
{"type":"about:blank","title":"Bad Request","status":400,"detail":"The request has invalid fields.",
 "code":"VALIDATION_FAILED","correlation_id":"9a4d05a9-...",
 "errors":[{"field":"amount","message":"must be greater than 0"},{"field":"currency","message":"must be IDR"}]}
```

| Status | When |
|---|---|
| 202 | Accepted, also for a replay |
| 400 | Invalid fields, malformed JSON, missing `Idempotency-Key` |
| 404 | Unknown transfer id |
| 413, 415 | Body over 16 KB, not `application/json` |
| 422 `INSUFFICIENT_BALANCE` | Not enough balance; nothing stored, so the same key can be tried again |
| 422 `MERCHANT_PAYOUTS_HELD` | The ledger check found a mismatch and holds this merchant's payouts; nothing stored |
| 422 `IDEMPOTENCY_KEY_REUSED` | Same key, different body |
| 503 | Database unreachable or too slow; retry with the same key |

## Assumptions

- Balance updates are serialized by a row lock: each deduction locks the merchant's balance, so
  requests for one merchant take turns and can never overdraw it. The database runs in its
  default mode (`READ COMMITTED`); this lock is all the balance needs.
- When a transfer fails, the money comes back as a separate refund (a `REFUND` ledger entry). The
  original deduction is not undone.
- Only the service writes to its database; nobody edits rows by hand (access control outside the
  service stops that). So a ledger entry never changes once written.
- One currency, IDR, in whole rupiah. Banks settle IDR payouts in whole units, so every stored
  amount is an integer.
- The maximum per transfer is 250,000,000 rupiah, the BI-FAST per-transaction limit
  (configurable).
- An `Idempotency-Key` stands for one transfer the merchant wants to make. Once accepted, the same
  key always returns that transfer. A declined, invalid or failed request stores nothing, so the
  same key can be tried again. Keys never expire.
- `reference_id` is the merchant's own reference and doesn't have to be unique; duplicates are
  caught by the idempotency key.
- The account holder's name is returned as sent; only the account number is masked.
- Paths are singular as specified (`/transfer`). A public API would use `/v1/transfers`.
- The payout provider remembers each key longer than the service keeps asking about a transfer
  (about 3 hours), so asking again never pays twice.
  Stripe, for example, keeps keys for at least 24 hours.
- The mock provider:
  - keeps everything in memory: a restart forgets every key, which a real provider wouldn't;
  - records the payout before its delay, like a bank that pays but answers late;
  - records nothing on a 503;
  - is reachable only inside the compose network;
  - gives random outcomes; the account endings above give repeatable cases.

## Out of scope

- **No authentication.** Anyone who knows a transfer id can read it (ids are UUIDv7, and responses
  leave out what isn't needed). In production: authenticate the merchant, query by id and merchant,
  and answer 404 for other merchants' transfers.
- **No message signing.** A request changed or replayed on the way would be accepted. In production,
  each request carries a signature (an HMAC of a timestamp and the body) that the API checks.
- **No tool for transfers the reconciler gave up on.** Their money stays held and the
  `reconcile_exhausted` alert lists them. Fixing one needs a command that reuses the worker's
  finish-and-refund code; editing rows by hand would skip the refund and the ledger entry.
- **The reconciler asks again by resending the payout request** with the same key, not through a
  status endpoint. The fake provider answers a repeated key with its stored result, the same as a
  status check. A real provider may not: Stripe, for example, can replay a stored error, so only a
  status check tells what happened.
- **Who resolved a ledger finding is free text.** Without logins anyone with Docker access can
  type any name; in production the name comes from the login system.
- **Alerts are log lines only.** `alert=outbox_backlog`, `alert=reconcile_exhausted`,
  `alert=ledger_mismatch`, `alert=provider_unrecognized_response` and `event=transfer_unknown`
  are structured fields for a log platform to alert on. Routing to chat or paging belongs in the
  monitoring platform (grouping, silencing, on-call), which isn't part of this setup.
- **No metrics endpoint.** What to measure is listed under future improvements.
- **Postgres and RabbitMQ run as single nodes.** Production needs a replicated setup: a Postgres
  primary with a standby, and 3 RabbitMQ nodes so quorum queues are actually replicated.
- **Account numbers are stored in plain text** in Postgres (masked everywhere else).
- **The mock provider's `/stats` and key store grow without bound**, which is fine for a demo.

## Future improvements

Correctness and money
- With a real provider, ask its status endpoint (or listen to its webhooks) instead of sending the
  payout request again.
- If a merchant's payouts stay on hold for days, cancel and refund the waiting transfers, and tell
  the merchant when a hold starts or ends.
- A command to close many ledger findings at once (useful after a bug in the check), with a
  preview first.
- A status history table, for a permanent timeline of each transfer.
- When two workers handle the same transfer and one already marked it `UNKNOWN`, let the other's
  clear answer finish it right away; today it waits for the next re-check.
- Let the resolve command re-check the merchant's ledger and refuse while it still doesn't match, so
  a hold can't be lifted by mistake.

Scale
- One merchant's deductions take turns on one row, which limits how fast a single very busy
  merchant can go. Beyond that, split its balance over several rows or group deductions.
- Lock the balance first and check it in Go (`SELECT ... FOR UPDATE`) when a deduction needs more
  rules, such as daily limits or fees, or moves money between several accounts.
- When provider retries need minutes, schedule them in RabbitMQ (a retry queue, or scheduled
  delivery in 4.4) instead of waiting inside the worker, so workers stay free.
- Send events to RabbitMQ in batches if the relay becomes the slowest part.
- Put a connection pooler (PgBouncer) in front of Postgres when replicas grow: each process may
  open up to 20 connections (`DB_MAX_OPEN_CONNS`), so 2 api replicas, the relay and 3 workers could
  need 120, above Postgres' default of 100. The 3-worker load test peaks at 12.
- Clean up sent outbox rows after some days, or split the table by date and drop old parts.
- On one very busy merchant, the link from each transfer to its merchant row adds Postgres
  bookkeeping (multixacts, about 4 per transfer under load, measured). Watch it, or drop that link:
  the ledger entry already ties the transfer to the merchant.
- Each round, the ledger check looks at every merchant to find the ones with new entries (about
  35 ms at 100,000 merchants). With many more merchants, mark a merchant as changed when its
  balance changes instead.

Operations
- Metrics: transfers by status, outbox lag, dead-letter queue depth, provider latency and
  timeouts, API latency, ledger check lag (oldest unchecked entry), with alert rules.
- A circuit breaker: when the provider keeps failing, stop taking messages for a while instead of
  hammering it, without marking any transfer `FAILED`.
- Limit how many requests each merchant can send, at the gateway.
- An alert on the age of the oldest open transfer, so a reconciler that fails on every run is
  noticed.
- Restart the relay when one of its loops stops making progress. The RabbitMQ client's publish can
  block forever if the broker stops reading, and `/healthz` reports it, but nothing in Docker
  Compose acts on it (a Kubernetes liveness probe would).
- Set the delivery limit through a RabbitMQ policy, which can change on a running queue. Today,
  changing `QUEUE_DELIVERY_LIMIT` on an existing broker stops the relay and worker from starting
  until the queue is deleted.

Security and data
- Encryption or tokenization of account numbers, database access auditing, and a retention and
  deletion policy.
