---
phase: 23-snapshot-create-orchestration-sync-gate
plan: 01
subsystem: snapshot
tags: [snapshot, sync-gate, blockstore, config]
requires:
  - pkg/blockstore/remote/remote.go::RemoteStore.Head
  - pkg/blockstore/errors.go::ErrBlockNotFound
  - pkg/blockstore/hashset.go::HashSet.Sorted
provides:
  - pkg/snapshot/syncgate.go::VerifyRemoteDurability
  - pkg/config/config.go::SnapshotConfig
  - pkg/config/config.go::SnapshotConfig.ApplyDefaults
  - pkg/config/config.go::SnapshotConfig.Validate
affects:
  - pkg/config/defaults.go (wires Snapshot.ApplyDefaults into top-level ApplyDefaults)
tech_stack:
  added: []
  patterns:
    - buffered-channel semaphore + select-on-cancel (model: pkg/adapter/base.go:247-256)
    - first-error-wins via sync.Once + context.WithCancel
    - external test package (snapshot_test) with hand-rolled RemoteStore fakes
    - GCConfig-mirror ApplyDefaults+Validate shape for SnapshotConfig
key_files:
  created:
    - pkg/snapshot/syncgate.go
    - pkg/snapshot/syncgate_test.go
    - pkg/config/snapshot_test.go
  modified:
    - pkg/config/config.go (SnapshotConfig block + field on top-level Config)
    - pkg/config/defaults.go (top-level ApplyDefaults wiring)
decisions:
  - D-23-06 honored: VerifyRemoteDurability fail-fast on first missing hash
  - D-23-07 honored: caller-supplied concurrency parameter
  - D-23-08 honored: no internal timeout; only caller ctx.Done() aborts
  - D-23-22 honored: SnapshotConfig{SyncGateConcurrency} default 16, range [1, 256]
metrics:
  duration: ~25 minutes
  completed: 2026-05-28
  tasks: 2/2
  commits: 3 (one test, two implementation)
---

# Phase 23 Plan 01: Snapshot Sync Gate + Config Knob Summary

Implemented the pure sync-gate probe (`snapshot.VerifyRemoteDurability`) plus
the operator-tunable `snapshot.sync_gate_concurrency` config knob. Both ship
in the same plan so plan 23-04's orchestration glue can land on a complete
foundation. No Runtime wiring yet — that's plan 23-04.

## Signatures Landed

```go
// pkg/snapshot/syncgate.go
func VerifyRemoteDurability(
    ctx context.Context,
    rs remote.RemoteStore,
    manifest *blockstore.HashSet,
    concurrency int,
) error

// pkg/config/config.go
type SnapshotConfig struct {
    SyncGateConcurrency int `mapstructure:"sync_gate_concurrency" yaml:"sync_gate_concurrency"`
}
func (c *SnapshotConfig) ApplyDefaults()
func (c *SnapshotConfig) Validate() error
```

## Behavior Matrix (7 enumerated + 1 added)

| Behavior | Test name | Notes |
|---|---|---|
| Happy path: all hashes present → nil | `TestVerifyRemoteDurability_HappyPath` | 32 hashes, concurrency 4 |
| Empty manifest → nil, no I/O | `TestVerifyRemoteDurability_EmptyManifest` | early return |
| Nil manifest → nil | `TestVerifyRemoteDurability_NilManifest` | nil-safe guard |
| Missing hash → wrapped `ErrBlockNotFound` naming the hash | `TestVerifyRemoteDurability_MissingHashFailFast` | `errors.Is` + substring check on error message |
| Non-NotFound I/O error propagates unchanged | `TestVerifyRemoteDurability_IOErrorPropagates` | `errors.Is(err, sentinel)` and NOT `ErrBlockNotFound` |
| Parent ctx cancel honored | `TestVerifyRemoteDurability_ContextCancelHonored` | blocking fake + race condition exercised |
| Concurrency bound observed via max-in-flight watermark | `TestVerifyRemoteDurability_ConcurrencyBound` | counting fake with high-water atomic |
| Non-positive concurrency clamps to 1 (no panic, no deadlock) | `TestVerifyRemoteDurability_ConcurrencyDefaultsOnNonPositive` | table-driven: 0, -1, -100 |
| Fail-fast actually cancels siblings (completion count < total) | `TestVerifyRemoteDurability_FailFastCancelsSiblings` | slow-miss fake; verifies cancel is observable, not just declared |

All 9 sub-tests PASS under `go test -race -count=1`.

## Config Knob

- **Name:** `snapshot.sync_gate_concurrency`
- **Default:** 16 (matches the existing `gc.sweep_concurrency` order-of-magnitude)
- **Range:** [1, 256] inclusive — values outside reject at config load
- **Wiring:** top-level `Config.Snapshot SnapshotConfig`, `cfg.Snapshot.ApplyDefaults()` wired into `ApplyDefaults(cfg)` alongside Syncer/GC

Config tests (5 funcs, 6 sub-cases in `TestSnapshotConfig_Validate_RangeBounds`):

| Test | Assertion |
|---|---|
| `TestSnapshotConfig_ApplyDefaults_Defaults16` | zero-value → 16 after top-level ApplyDefaults |
| `TestSnapshotConfig_ApplyDefaults_ExplicitValuePreserved` | explicit 64 stays 64 |
| `TestSnapshotConfig_ApplyDefaults_NegativeIsCorrected` | -1 → 16 (matches `c <= 0` defaulting) |
| `TestSnapshotConfig_Validate_RangeBounds` | table: -1, 0, 257 reject; 1, 16, 256 accept |
| `TestSnapshotConfig_ValidatePassesOnDefaults` | the defaulted config always passes Validate |

All PASS under `go test -race -count=1` on `./pkg/config/...`.

## Implementation Notes

- **Cancellation semantics:** the helper derives `errCtx, cancel := context.WithCancel(ctx)` and uses a `sync.Once` to capture the first error. Workers observing `context.Canceled`/`context.DeadlineExceeded` from a sibling-triggered cancel do NOT overwrite `firstErr`, so the original miss / I/O error is what surfaces. After `wg.Wait`, if `firstErr` is still nil and the parent ctx was cancelled, `ctx.Err()` is returned (covers the dispatch-loop-break-before-all-hashes case).
- **Error wrap shape:** matches the interfaces block exactly — `fmt.Errorf("snapshot: remote durability verify: missing hash %s: %w", hash, blockstore.ErrBlockNotFound)`. Non-NotFound errors use a parallel "head hash %s: %w" form with the underlying error so the runtime layer can preserve causal chains.
- **Logging:** `logger.Debug` for the expected miss path (per CLAUDE.md invariant 6), `logger.Error` for the unexpected I/O-error path. Hash is NOT redacted in logs/errors per Phase 23 specifics note (operators need to grep).
- **Test fakes:** hand-rolled `RemoteStore` implementations (errRemote, blockingRemote, countingRemote, slowMissingRemote) because every behavior the plan calls out needs richer instrumentation than the memory `Store` offers (atomic in-flight watermarks, per-call gating, controlled mid-stream errors). The happy-path tests still use the canonical `remote/memory.Store`.

## Deviations from PATTERNS.md

None functional — implementation tracks the PATTERNS.md anchor list verbatim:

- Bounded-parallel + fail-fast cancellation → `pkg/adapter/base.go:247-256` semaphore shape applied
- `Head` vs `ErrBlockNotFound` discrimination → `errors.Is(err, blockstore.ErrBlockNotFound)` per `pkg/blockstore/errors.go`
- Sentinel-error wrap with `%w` → matches `pkg/snapshot/manifest.go:33-41`
- Test file placement → external test package `snapshot_test` (matches `manifest_test.go`)
- `SnapshotConfig` block → modeled verbatim on `GCConfig` (lines 132-205); only difference is the validation message format (`"snapshot.sync_gate_concurrency must be in [1, 256] (got %d)"`), which mirrors the same `fmt.Errorf("X must be ... (got %d)", v)` shape

One minor codebase observation (NOT a deviation; just for future plan-checkers): `SyncerConfig.Validate` and `GCConfig.Validate` are defined but NOT currently wired into the top-level `Validate(cfg *Config)` function — only `Blockstore.Validate()` is called there. `SnapshotConfig.Validate` followed the same pattern (defined as a method, not invoked from top-level Validate). If a future plan wants to fail-closed at config load, all three sub-Validate calls should be added to `pkg/config/validation.go:Validate` in a single sweep.

## Auto-Fixes / Auth Gates

None. Plan executed exactly as written.

## Success Criteria

- [x] ROADMAP SC-1 satisfied at the unit level: `VerifyRemoteDurability` correctly checks all manifest hashes against remote store (9 sub-tests assert the full behavior matrix)
- [x] Operator-tunable knob landed: `snapshot.sync_gate_concurrency` default 16
- [x] ORCH-01 first half implemented (pure helper); integration assertion lives in plan 23-06
- [x] Plan is independently committable per D-23-23: 3 commits (test, impl, config) yield a buildable tree at every checkpoint
- [x] `gofmt -s -l pkg/snapshot pkg/config` clean; `go vet ./pkg/snapshot/... ./pkg/config/...` exits 0
- [x] `go build ./...` clean

## Verification Commands

```bash
# unit gate
go test ./pkg/snapshot/... -race -count=1
go test ./pkg/config/...   -race -count=1

# format gate
gofmt -s -l pkg/snapshot pkg/config

# vet gate
go vet ./pkg/snapshot/... ./pkg/config/...

# signature grep (from PLAN.md verification block)
grep -n "func VerifyRemoteDurability" pkg/snapshot/syncgate.go
grep -n "type SnapshotConfig"          pkg/config/config.go
grep -n "sync_gate_concurrency"        pkg/config/config.go
```

## Self-Check: PASSED

- `pkg/snapshot/syncgate.go` exists
- `pkg/snapshot/syncgate_test.go` exists
- `pkg/config/snapshot_test.go` exists
- Commits present: `bce7b5ac` (test), `36685ac9` (impl), `e61015a4` (config)
