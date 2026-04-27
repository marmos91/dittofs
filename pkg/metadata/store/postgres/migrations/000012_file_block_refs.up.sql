-- Phase 12 PR-A (META-01): file_block_refs join table.
--
-- Stores FileAttr.Blocks []BlockRef for files. Per D-01 we use a separate
-- table (not JSONB on files) to avoid TOAST write amplification on the
-- VM-primary workload — random 4 KiB writes touch 1-2 BlockRef rows
-- instead of rewriting a ~1.5 MB JSONB blob.
--
-- PK is (file_id, "offset") with INCLUDE(size, hash) covering index per
-- D-02 — lets the engine fetch all four columns from index leaves
-- without a heap fetch on the cold-cache read path.
--
-- FK file_id -> files(id) ON DELETE CASCADE (D-03) is a safety net.
-- Engine still decrements file_blocks.RefCount before deleting the file
-- — the cascade only catches engine-bug paths.
--
-- Hash column is BYTEA (32 bytes) per D-04, not hex TEXT — half the
-- storage of hex, faster btree compares, native binary scan in
-- index-only path.

CREATE TABLE IF NOT EXISTS file_block_refs (
    file_id  UUID    NOT NULL,
    "offset" BIGINT  NOT NULL,
    size     INTEGER NOT NULL,
    hash     BYTEA   NOT NULL,
    PRIMARY KEY (file_id, "offset") INCLUDE (size, hash),
    CONSTRAINT fk_file_block_refs_file
        FOREIGN KEY (file_id)
        REFERENCES files(id)
        ON DELETE CASCADE
);

-- Reverse index: live-set lookup for the Phase 13 file-level dedup
-- short-circuit (BSCAS-04/05) and the Phase 12 INV-02 audit. Same
-- shape as idx_file_blocks_hash from Phase 11.
CREATE INDEX IF NOT EXISTS idx_file_block_refs_hash
    ON file_block_refs(hash);
