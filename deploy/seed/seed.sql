-- Re-runnable: does nothing once merchant 1 exists.
WITH merchant AS (
    INSERT INTO merchants (id, name, balance, version)
    VALUES (1, 'Demo Merchant', :'initial_balance'::BIGINT, 1)
    ON CONFLICT (id) DO NOTHING
    RETURNING id, balance, version
)
INSERT INTO balance_entries (merchant_id, type, amount, balance_after, merchant_version)
SELECT id, 'OPENING', balance, balance, version FROM merchant;
