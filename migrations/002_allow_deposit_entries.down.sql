-- Fails while DEPOSIT entries exist, rather than deleting money history.
ALTER TABLE balance_entries
    DROP CONSTRAINT balance_entries_type_valid,
    DROP CONSTRAINT balance_entries_amount_sign,
    DROP CONSTRAINT balance_entries_transfer_link,
    ADD CONSTRAINT balance_entries_type_valid CHECK (type IN ('OPENING', 'DEBIT', 'REFUND')),
    ADD CONSTRAINT balance_entries_amount_sign CHECK (
        (type = 'DEBIT' AND amount < 0) OR (type IN ('OPENING', 'REFUND') AND amount > 0)
    ),
    ADD CONSTRAINT balance_entries_transfer_link CHECK ((type = 'OPENING') = (transfer_id IS NULL));
