-- Persist GrantedAccess (DACL-evaluated per-bit mask) on durable handles
-- (#552 review follow-up). Preserves the exact granted set across reconnect
-- so FileAccessInformation / FileAllInformation return identical rights
-- after re-establishment, mirroring Samba's smbXsrv_open_global semantics.
-- Pre-existing rows default to 0, which triggers the in-code fallback to
-- the resolved DesiredAccess for forward compatibility.

ALTER TABLE durable_handles ADD COLUMN IF NOT EXISTS granted_access BIGINT NOT NULL DEFAULT 0;
