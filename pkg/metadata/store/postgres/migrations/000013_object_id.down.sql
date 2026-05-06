-- ============================================================================
-- WARNING - DESTRUCTIVE: drops files.object_id column.
--
-- Every Phase 13 ObjectID payload is permanently deleted. Files written
-- post-Phase-13 will lose their Merkle-root fingerprint; subsequent
-- file-level dedup short-circuits on those files become impossible
-- until each file re-quiesces (post-rollback writes recompute on next
-- coordinator hook). This is safe for the v0.15.0 dual-read window but
-- operationally equivalent to a full re-fingerprint pass.
--
-- Per Phase 12 D-07 (migration discipline) this is forward-only at the
-- operational level — only run pre-deploy or with an explicit Phase-14
-- migration plan.
-- ============================================================================

DROP INDEX IF EXISTS files_object_id_idx;
ALTER TABLE files DROP COLUMN IF EXISTS object_id;
