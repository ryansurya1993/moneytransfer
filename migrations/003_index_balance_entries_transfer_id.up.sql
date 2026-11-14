-- A transfer's debit and refund are looked up by transfer_id.
-- CONCURRENTLY keeps the ledger writable while the index builds; it must be the only statement in the file.
-- No IF NOT EXISTS: a failed build leaves an invalid index that it would silently accept.
CREATE INDEX CONCURRENTLY balance_entries_transfer_id_idx ON balance_entries (transfer_id);
