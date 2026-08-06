-- Restore the (broken) UNIQUE constraint. DDL copied from
-- 000005_durable_handles.up.sql.
--
-- WARNING: this rollback FAILS whenever a file currently holds more than one
-- durable handle — those rows are exactly what the unique index forbids, and
-- they are legitimate state once multiple opens per file persist. The whole
-- file runs as one implicit transaction, so the failure is atomic and the
-- non-unique index stays in place; the schema is left usable, but the
-- rollback does not complete until the duplicate rows are removed.
-- Down only intended for migration tooling completeness; do NOT run on a
-- live deployment.
DROP INDEX IF EXISTS idx_durable_handles_file_id;
CREATE UNIQUE INDEX IF NOT EXISTS idx_durable_handles_file_id ON durable_handles(file_id);
