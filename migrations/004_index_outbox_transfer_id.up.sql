-- When a transfer's events were published is looked up by transfer_id.
-- Built CONCURRENTLY for the same reasons as 003.
CREATE INDEX CONCURRENTLY outbox_transfer_id_idx ON outbox (transfer_id);
