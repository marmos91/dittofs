-- Phase 13 PR-A (META-02 / BSCAS-04): files.object_id Merkle-root column.
--
-- Stores FileAttr.ObjectID for files. Per Phase 13 D-12 we use a column
-- on the existing files row (not a separate index table) because the
-- ObjectID is naturally one-to-one with the file and read on every
-- GetFile alongside the rest of the row.
--
-- Partial UNIQUE index (WHERE object_id IS NOT NULL) provides the
-- BSCAS-05 lookup AND enforces D-14 first-committer-wins on concurrent
-- quiesce: the loser's INSERT/UPDATE rejects with unique-violation
-- (SQLSTATE 23505), detects, swaps to target's BlockRef list, and
-- retries. Legacy and partially-flushed files (object_id NULL) are
-- skipped by the index so they never collide on the all-zero sentinel.
--
-- BYTEA (32 bytes) per Phase 12 D-04 — half the storage of hex,
-- faster btree compares, native binary scan.

ALTER TABLE files ADD COLUMN IF NOT EXISTS object_id BYTEA;

CREATE UNIQUE INDEX IF NOT EXISTS files_object_id_idx
    ON files(object_id)
    WHERE object_id IS NOT NULL;
