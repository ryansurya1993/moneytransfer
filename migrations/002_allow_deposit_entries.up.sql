-- A deposit adds money from outside, such as a merchant top-up, so like OPENING it has no transfer.
ALTER TABLE balance_entries
    DROP CONSTRAINT balance_entries_type_valid,
    DROP CONSTRAINT balance_entries_amount_sign,
    DROP CONSTRAINT balance_entries_transfer_link,
    ADD CONSTRAINT balance_entries_type_valid CHECK (type IN ('OPENING', 'DEPOSIT', 'DEBIT', 'REFUND')),
    ADD CONSTRAINT balance_entries_amount_sign CHECK (
        (type = 'DEBIT' AND amount < 0) OR (type IN ('OPENING', 'DEPOSIT', 'REFUND') AND amount > 0)
    ),
    ADD CONSTRAINT balance_entries_transfer_link CHECK ((type IN ('OPENING', 'DEPOSIT')) = (transfer_id IS NULL));
