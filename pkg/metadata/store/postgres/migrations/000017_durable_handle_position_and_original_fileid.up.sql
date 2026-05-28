-- Persist PositionInfo + OriginalFileID on durable handles (#663 follow-up to #661).
--
-- DH V1 reconnect plumbing added two fields to lock.PersistedDurableHandle that
-- the memory backend already round-trips and badger picks up via JSON:
--
--   * PositionInfo  — FILE_POSITION_INFORMATION CurrentByteOffset
--                     (MS-FSCC 2.4.32) captured at disconnect. Restoring this
--                     on reconnect makes SET/GET FilePositionInformation
--                     survive the disconnect (smb2.durable-open.file-position).
--
--   * OriginalFileID — full 16-byte FileID (persistent + volatile) from the
--                     original CREATE response. The primary `file_id` column
--                     zeros the volatile half so DHnC lookup matches the spec
--                     (MS-SMB2 §3.2.4.4 client sends Data.Volatile=0). On
--                     successful reconnect the handler restores OriginalFileID
--                     into the new OpenFile so byte-range locks (which key on
--                     the OpenID derived from FileID) stay valid across the
--                     disconnect (smb2.durable-open.lock-{oplock,lease}).
--
-- Without these columns the postgres profile silently regresses DH V1: file
-- position reports 0 after reconnect and BR locks are orphaned because
-- OriginalFileID always decodes to zero, forcing volatile-half regeneration
-- and a fresh OpenID.
--
-- Pre-existing rows default to 0 / all-zero bytes. The reconnect handler
-- already treats a zero OriginalFileID as "fall back to file_id" for
-- backwards compatibility, so existing handles keep working.

ALTER TABLE durable_handles
    ADD COLUMN IF NOT EXISTS position_info BIGINT NOT NULL DEFAULT 0;

ALTER TABLE durable_handles
    ADD COLUMN IF NOT EXISTS original_file_id BYTEA NOT NULL DEFAULT '\x00000000000000000000000000000000'::bytea;

ALTER TABLE durable_handles
    ADD CONSTRAINT valid_original_file_id CHECK (length(original_file_id) = 16);
