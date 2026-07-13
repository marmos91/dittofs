-- Persist the SMB2 client GUID on durable handles so lease-backed durable
-- reconnect can reject a reconnect from a different client GUID (#1663).
--
-- ClientGUID (#432) was added to the in-memory handle but never given a column
-- here, so scanDurableHandle always returned a zero ClientGUID. That
-- short-circuits leaseReconnectClientGUIDMismatch, wrongly returning
-- NT_STATUS_OK instead of NT_STATUS_OBJECT_NAME_NOT_FOUND for
-- smb2.durable-open.reopen1a-lease / durable-v2-open.reopen1a-lease. NULL for
-- pre-existing rows decodes to a zero ClientGUID, matching the prior behavior.
ALTER TABLE durable_handles ADD COLUMN client_guid BLOB;
