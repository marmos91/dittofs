-- ============================================================================
-- WARNING - DESTRUCTIVE: drops shares.block_layout column.
--
-- Every per-share block-layout flag is permanently deleted. After this
-- runs, the engine cannot tell which shares are migrated to the v0.15
-- CAS layout and which still need the dual-read shim. Subsequent
-- writes via a v0.15+ binary will treat every share as if it were
-- ParseBlockLayout("") → legacy, which is the safe default but
-- erases the operator's prior migration progress.
--
-- T-14-01-03: this is operator-initiated, only used during dev/test
-- rollback. Production rollback is the Phase-15 deletion path.
-- ============================================================================

ALTER TABLE shares
    DROP COLUMN IF EXISTS block_layout;
