# Step 4 legacy cleanup — receipt

**Commit:** 132a3d18a `refactor(block): delete pkg/block/local/fs, hold journal.Store directly`
**Date:** 2026-09-11
**Lane:** step 4 (legacy cleanup) of `.planning/2026-09-01-block-dataflow-MASTER-PLAN.md` §4, plus the
docs-cleanup portion of lane L from `.planning/2026-09-11-step3-execution-PLAN.md`.

## What landed

### Code (pkg/block)

- `pkg/block/local/fs` deleted (1766 LOC: fs.go, legacy.go, legacy_reader.go, legacy_migrate.go,
  legacy_test.go, legacy_migrate_test.go, format_test.go, disk_used_test.go). Net repo
  −2461 lines across 71 files.
- `pkg/block/local/local.go` re-declared: `LocalStore` is now in journal's vocabulary —
  `journal.FileID`, `journal.ReadState`, `journal.CarveOptions/CarveResult`, `journal.EvictResult`,
  `journal.Stats` — so `*journal.Store` satisfies it directly. `Stats()`, `Durable()/SetDurable()`,
  `FileCount()` re-declared; the dead `Start`-launches-loops contract is now a documented no-op.
- `pkg/block/journal/admin.go` (new): `MaxLocalBytes()`, `Durable()/SetDurable()` (the old adapter's
  hardcoded `Durable() == true` was an unread override — now the report is a store fact),
  `Healthcheck()`, no-op `Start()`. `Store.durable atomic.Bool` defaults true at Open.
- `journal.Config.MaxLogBytes` and `journal.Stats.MaxLogBytes`: max_log_bytes threads as a Stats
  size hint only — it does not gate writes.
- `engine.Store.MaxLocalBytes()` (new, type-asserted structurally), `LocalStats()` returns
  `journal.Stats`, `DurableExtent`'s internal assertion matches the journal signature.
- `engine/offline.go` `coldRangeReporter` re-declared with the journal signature.

### Wiring (pkg/controlplane)

- `shares.CreateLocalStoreFromConfig`: the `migrateLegacy` param is dead and removed;
  `openJournalStore` (new) opens `journal/` under the share dir with `journal.Config` directly.
  `FSStoreOptions` is gone.
- `shares/blockstore_config.go`: the detached legacy local-only migration goroutine and the
  `legacyLocalOnlyMigrator` interface are removed with the machinery they drove.
- `cmd/dfs/commands/start.go` + `runtime/init.go`: the `fs.ErrLegacyLocalFormat` boot guard is
  dropped. Accepted per master-plan decision D4: no production stores exist in field, so a legacy
  pre-journal directory opening as an empty journal is not a stranded-bytes hazard.

### Docs

- `docs/internals/architecture.md`: `*journal.Store` IS the byte cache (no adapter); the stale
  `ErrLegacyLayoutDetected` / exit-78 boot-guard passage now documents the format stamp and the
  deletion.
- `docs/internals/implementing-stores.md`: reference implementation points at `pkg/block/journal/`
  (the earlier bad `pkg/block/n/` sed corruption is fixed).
- `docs/internals/rfc-block-dataflow.md`: status line reflects landed lanes H + C.

## Verification

- `go build ./...`, `go vet ./...`, `gofmt -l` — clean.
- `go test ./pkg/block/engine/` 61.8s PASS; `go test -race ./pkg/block/journal/` 39.1s PASS;
  `go test -race ./pkg/block/engine/` 181.6s PASS.
- `go test ./pkg/controlplane/... ./cmd/... ./internal/... ./pkg/metrics/... ./pkg/config/...` PASS
  (fixtures, boot guard, shares, blockstoreprobe).

## Residual (out of scope, still open)

- Lane F (Flush+Run+AfterFile seam) and lane L's layout+tests remainder — blocked on F per the
  step3 plan.
- Step-4 remainder: legacy-CAS object layout, vestigial config knobs (use_append_log,
  rollup_workers, stabilization_ms, orphan_log_min_age_seconds), Locator's standalone form,
  ContentHash's two legacy JSON encodings, FileChunk's multi-row-per-hash tolerance.
- The `memory.MemoryStore.Hydrate` notAfter gate is still accepted-and-ignored (ponytail note in
  place).
