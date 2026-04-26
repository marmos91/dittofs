-- Phase 11 WR-4-01: drop the UNIQUE constraint on file_blocks.hash.
--
-- The Phase 11 dedup short-circuit (engine.uploadOne) intentionally produces
-- multiple FileBlock rows that share the same ContentHash whenever two file
-- regions hash-match (e.g. an all-zero block reused across distinct VM
-- images — DittoFS's documented primary workload). The original 000010
-- migration created idx_file_blocks_hash as a partial UNIQUE index on
-- (hash WHERE hash IS NOT NULL); this rejected the second writer with
-- "duplicate key value violates unique constraint", causing engine.uploadOne
-- to leave the FileBlock in Syncing forever and the syncer's janitor to
-- requeue it on every claim_timeout. The donor's RefCount was permanently
-- leaked.
--
-- Per the FileBlockStore.PutFileBlock contract pinned in IN-3-02 (see
-- pkg/blockstore/store.go), the hash column is NOT a uniqueness constraint
-- at the contract level — backends MUST tolerate cross-row hash duplicates
-- silently. Memory + Badger satisfied the contract via silent overwrite of
-- their hash→id maps; only Postgres broke it.
--
-- Resolution: replace the partial UNIQUE index with a regular partial index.
-- FindFileBlockByHash retains its lookup speed (the index still exists);
-- INSERT no longer rejects on cross-row hash collisions. Memory + Badger
-- already produce arbitrary-row results for the lookup; Postgres now
-- matches.
--
-- Fresh installs running 000010 directly already get the non-unique form
-- because we reissue 000010 with CREATE INDEX (not CREATE UNIQUE INDEX) and
-- the IF NOT EXISTS guard makes the down/up cycle idempotent. This 000011
-- exists for deployments that ran 000010 before the fix.

DROP INDEX IF EXISTS idx_file_blocks_hash;

CREATE INDEX IF NOT EXISTS idx_file_blocks_hash
    ON file_blocks(hash) WHERE hash IS NOT NULL;
