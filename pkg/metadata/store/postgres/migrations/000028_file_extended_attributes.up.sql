-- Persist extended attributes (EAs) on files (SMB FILE_FULL_EA_INFORMATION,
-- MS-FSCC §2.4.15).
--
-- An SMB client may attach named extended attributes to a file via SET_INFO
-- FileFullEaInformation or an SMB2_CREATE_EA_BUFFER create context, and read
-- them back via QUERY_INFO FileFullEaInformation. The EA set is a small
-- name->value map (names case-insensitive, values raw bytes). The memory
-- backend holds it as a typed map and badger rides it on the JSON FileAttr
-- blob; only the postgres profile needs an explicit column.
--
-- Stored as JSONB (object keyed by EA name, value is a base64 string per Go's
-- encoding/json []byte marshalling), mirroring the existing `acl` column.
-- NULL means "no EAs"; pre-existing rows default to NULL.

ALTER TABLE files
    ADD COLUMN IF NOT EXISTS eas JSONB;
