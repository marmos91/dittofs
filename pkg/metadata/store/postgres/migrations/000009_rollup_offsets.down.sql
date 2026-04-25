-- Phase 10 LSL-05 rollback: drop the per-file rollup_offset table.
-- Safe to run on a freshly-migrated database; data loss is expected on
-- an active deployment (the table holds durable state for in-flight
-- append-log rollups).

DROP TABLE IF EXISTS rollup_offsets;
