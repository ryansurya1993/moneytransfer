CREATE TABLE merchants (
    id                  BIGINT      PRIMARY KEY,
    name                TEXT        NOT NULL,
    balance             BIGINT      NOT NULL,
    -- Change counter stamped on each ledger entry; not used for optimistic locking.
    version             BIGINT      NOT NULL DEFAULT 0,
    -- Set while the ledger check has open findings for the merchant: no new payouts go out.
    payouts_held_reason TEXT,
    payouts_held_at     TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT merchants_balance_non_negative CHECK (balance >= 0),
    CONSTRAINT merchants_payouts_hold_complete CHECK ((payouts_held_reason IS NULL) = (payouts_held_at IS NULL))
);

CREATE TABLE transfers (
    id                    UUID        PRIMARY KEY,
    merchant_id           BIGINT      NOT NULL,
    idempotency_key       TEXT        NOT NULL,
    request_hash          TEXT        NOT NULL,
    amount                BIGINT      NOT NULL,
    currency              TEXT        NOT NULL,
    bank_code             TEXT        NOT NULL,
    account_number        TEXT        NOT NULL,
    account_holder_name   TEXT        NOT NULL,
    reference_id          TEXT,
    description           TEXT,
    status                TEXT        NOT NULL,
    failure_code          TEXT,
    provider_reference    TEXT,
    attempts              INT         NOT NULL DEFAULT 0,
    reconcile_attempts    INT         NOT NULL DEFAULT 0,
    last_error            TEXT,
    correlation_id        TEXT        NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    processing_started_at TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    ledger_checked_at     TIMESTAMPTZ,
    CONSTRAINT transfers_merchant_fk FOREIGN KEY (merchant_id) REFERENCES merchants (id),
    CONSTRAINT transfers_merchant_idempotency_key_uniq UNIQUE (merchant_id, idempotency_key),
    CONSTRAINT transfers_amount_positive CHECK (amount > 0),
    CONSTRAINT transfers_currency_idr CHECK (currency = 'IDR'),
    CONSTRAINT transfers_status_valid CHECK (status IN ('PENDING', 'PROCESSING', 'SUCCESS', 'FAILED', 'UNKNOWN'))
);

-- For the reconciler's sweep.
CREATE INDEX transfers_open_idx ON transfers (status, updated_at)
    WHERE status IN ('PENDING', 'PROCESSING', 'UNKNOWN');

-- Finished transfers the ledger check hasn't looked at yet; rows leave the index once checked.
CREATE INDEX transfers_ledger_unchecked_idx ON transfers (completed_at)
    WHERE ledger_checked_at IS NULL AND status IN ('SUCCESS', 'FAILED');

CREATE TABLE balance_entries (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL,
    transfer_id      UUID,
    type             TEXT        NOT NULL,
    amount           BIGINT      NOT NULL,
    balance_after    BIGINT      NOT NULL,
    merchant_version BIGINT      NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT balance_entries_merchant_fk FOREIGN KEY (merchant_id) REFERENCES merchants (id),
    CONSTRAINT balance_entries_transfer_fk FOREIGN KEY (transfer_id) REFERENCES transfers (id),
    CONSTRAINT balance_entries_merchant_version_uniq UNIQUE (merchant_id, merchant_version),
    CONSTRAINT balance_entries_type_valid CHECK (type IN ('OPENING', 'DEPOSIT', 'DEBIT', 'REFUND')),
    CONSTRAINT balance_entries_amount_sign CHECK (
        (type = 'DEBIT' AND amount < 0) OR (type IN ('OPENING', 'DEPOSIT', 'REFUND') AND amount > 0)
    ),
    -- OPENING and DEPOSIT (a top-up) bring money from outside, so they belong to no transfer.
    CONSTRAINT balance_entries_transfer_link CHECK ((type IN ('OPENING', 'DEPOSIT')) = (transfer_id IS NULL)),
    CONSTRAINT balance_entries_balance_after_non_negative CHECK (balance_after >= 0),
    -- At most one debit and one refund per transfer; also the index for looking them up.
    CONSTRAINT balance_entries_transfer_type_uniq UNIQUE (transfer_id, type)
);

CREATE TABLE outbox (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id    UUID        NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    correlation_id TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    CONSTRAINT outbox_transfer_fk FOREIGN KEY (transfer_id) REFERENCES transfers (id)
);

CREATE INDEX outbox_unsent_idx ON outbox (id) WHERE published_at IS NULL;
CREATE INDEX outbox_transfer_id_idx ON outbox (transfer_id);

-- The last ledger entry the ledger check verified per merchant; it reads only entries after it.
CREATE TABLE ledger_checkpoints (
    merchant_id      BIGINT      PRIMARY KEY,
    merchant_version BIGINT      NOT NULL,
    balance_after    BIGINT      NOT NULL,
    checked_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_checkpoints_merchant_fk FOREIGN KEY (merchant_id) REFERENCES merchants (id)
);

CREATE TABLE ledger_findings (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id      BIGINT      NOT NULL,
    rule             TEXT        NOT NULL,
    merchant_version BIGINT,
    transfer_id      UUID,
    found_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at      TIMESTAMPTZ,
    resolved_by      TEXT,
    resolution_note  TEXT,
    CONSTRAINT ledger_findings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES merchants (id),
    CONSTRAINT ledger_findings_transfer_fk FOREIGN KEY (transfer_id) REFERENCES transfers (id),
    CONSTRAINT ledger_findings_rule_valid CHECK (rule IN (
        'VERSION_GAP', 'BALANCE_CHAIN', 'MERCHANT_BALANCE', 'DEBIT_AMOUNT',
        'DEBIT_MISSING', 'REFUND_AMOUNT', 'REFUND_NOT_FAILED', 'REFUND_MISSING'
    )),
    CONSTRAINT ledger_findings_resolution_complete CHECK (
        (resolved_at IS NULL) = (resolved_by IS NULL) AND (resolved_at IS NULL) = (resolution_note IS NULL)
    )
);

-- A problem the check sees again on its next round is recorded once while open.
-- Once resolved, the same problem found again is a new finding and holds the merchant again.
CREATE UNIQUE INDEX ledger_findings_open_problem_uniq
    ON ledger_findings (merchant_id, rule, merchant_version, transfer_id) NULLS NOT DISTINCT
    WHERE resolved_at IS NULL;

CREATE INDEX ledger_findings_open_idx ON ledger_findings (merchant_id) WHERE resolved_at IS NULL;
