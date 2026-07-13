-- Persist the SMB2 client GUID on durable handles so lease-backed durable
-- reconnect can be rejected when it comes from a different client GUID
-- (smbtorture durable-open.reopen1a-lease / durable-v2-open.reopen1a-lease).
-- Older rows get NULL, which decodes to a zero ClientGUID — the same
-- backward-compatible "cannot verify → allow" behaviour as before this column.
ALTER TABLE durable_handles ADD COLUMN client_guid BYTEA;
