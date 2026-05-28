---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 04
subsystem: blockstore/engine
tags: [gc, hold-provider, snapshot, fail-closed]
requires:
  - "pkg/blockstore/engine/gc.go (markPhase / Options / MetadataReconciler)"
  - "pkg/blockstore/engine/gcstate.go (GCState.Add / FlushAdd)"
provides:
  - "engine.HoldProvider interface (D-02 signature)"
  - "engine.Options.HoldProvider field (nil-safe)"
  - "markPhase wiring that streams held hashes into the same GCState live set as EnumerateFileBlocks"
affects:
  - "pkg/blockstore/engine/gc.go"
  - "pkg/blockstore/engine/gc_test.go"
tech_stack:
  added: []
  patterns:
    - "Interface injection via Options struct (matches MetadataReconciler pattern)"
    - "Streaming callback signature (mirrors metadata.MetadataStore.EnumerateFileBlocks for verbatim loop reuse)"
    - "Fail-closed error propagation (HoldProvider error wraps via `hold provider: %w`, routed through the existing mark-abort path)"
key_files:
  created: []
  modified:
    - "pkg/blockstore/engine/gc.go"
    - "pkg/blockstore/engine/gc_test.go"
decisions:
  - "Followed CONTEXT D-01: single live set, single sweep — HoldProvider hashes go into gcs.Add() alongside FileBlock hashes; FlushAdd runs once after both streams."
  - "Followed CONTEXT D-02: signature exactly `HeldHashes(ctx, remoteEndpointID string, shares []string, fn func(blockstore.ContentHash) error) error`."
  - "Followed CONTEXT D-03: per-remote scoping is encoded in the (remoteEndpointID, shares) args — the engine forwards Options.RemoteEndpointID / Options.Shares verbatim; per-remote provider construction is Plan 22-05's job."
  - "Followed INV-04: any non-nil error from HeldHashes aborts the run before sweep — orphan-not-deleted preferred over live-data-deleted."
  - "Cleanly deleted the legacy v0.13.0 BackupHoldProvider negative-symbol test rather than carrying a compat assertion (matches the project's `no compat shims` rule). The protection is now positive: the engine has no v0.13.0 symbol because the test seeding a `HoldProvider` would not compile against it."
metrics:
  duration: "~25 min"
  completed: "2026-05-28T07:32:03Z"
---

# Phase 22 Plan 04: Engine GC Hold-Provider Injection Summary

GC mark phase now accepts a pluggable `HoldProvider` so snapshot-held hashes join the live set BEFORE FlushAdd and survive sweep without per-share FileBlock references.

## Interface shape

```go
type HoldProvider interface {
    HeldHashes(
        ctx context.Context,
        remoteEndpointID string,
        shares []string,
        fn func(blockstore.ContentHash) error,
    ) error
}
```

- Declared in `pkg/blockstore/engine/gc.go` immediately after `MultiShareReconciler`.
- Optional via `Options.HoldProvider` (nil-safe).
- Errors fail closed via the standard mark-abort path: `fmt.Errorf("hold provider: %w", err)` → `CollectGarbage` wraps it into `recordGCError("mark: ...")`, sweep does not run, `last-run.json` records the failure.

## markPhase wiring point

`markPhase` now accepts `(hold HoldProvider, remoteEndpointID string, shares []string)` and runs the HoldProvider callback AFTER all per-share `EnumerateFileBlocks` loops finish but BEFORE `gcs.FlushAdd()`. The callback re-uses the same `gcs.Add(h)` + `stats.HashesMarked++` shape as the enumeration callback.

```
for each share:
    EnumerateFileBlocks → cb (gcs.Add + stats++)
if hold != nil:
    hold.HeldHashes(ctx, remoteEndpointID, shares, cb)   // same cb shape
gcs.FlushAdd()
```

Per CONTEXT: held hashes MUST be visible to sweep's `gcs.Has()` lookups, so the single `FlushAdd` after both streams is load-bearing.

The mark-phase log line gained `hold_provider=<bool>` so SREs can correlate runs that had a provider attached vs. runs that did not.

## Tests (gc_test.go)

| Test | Asserts |
|------|---------|
| `TestGCMarkSweep_NoSnapshotHoldProvider` | nil HoldProvider preserves the pre-Phase-22 behavior verbatim |
| `TestGCMarkSweep_SnapshotHoldProvider` | held hashes land in the live set alongside FileBlock hashes — referenced/held/orphan CAS objects each get the right disposition |
| `TestGCMarkSweep_HoldProvider_ErrorFailsClosed` | HoldProvider error aborts the run pre-sweep; orphan that would have been deleted stays put; `deleteCountingRemote` records zero Deletes |

Two new local fixtures: `stubHoldProvider` (streams a fixed slice) and `stubErrHoldProvider` (always errors). Both are white-box (`package engine`) and use stdlib `t.Errorf` / `t.Fatalf` exclusively (no testify).

## Verification

```
go vet ./pkg/blockstore/...                                                 OK
go build ./pkg/blockstore/...                                               OK
go test ./pkg/blockstore/engine/... -count=1 -race                          OK (8.119s)
! grep BackupHoldProvider pkg/blockstore/engine/gc.go pkg/blockstore/engine/gc_test.go   OK
```

## Deviations from Plan

None — plan executed exactly as written. The Task 2 action specified two trailing args (`remoteEndpointID`, `shares`) on `markPhase`; both are threaded verbatim from `Options.RemoteEndpointID` / `Options.Shares` at the `CollectGarbage` call site. A pre-existing local variable named `shares` inside `markPhase` was renamed to `reconcilerShares` to avoid shadowing the new parameter — purely mechanical.

## Threat Flags

None — no new network endpoints, no new auth paths, no new schema changes at trust boundaries. Threat register entries T-22-04-01 / T-22-04-02 / T-22-04-03 / T-22-04-SC are mitigated/accepted per the plan and remain unchanged.

## Self-Check: PASSED

- FOUND: pkg/blockstore/engine/gc.go (modified — HoldProvider + Options field + markPhase wiring)
- FOUND: pkg/blockstore/engine/gc_test.go (modified — three new tests + two fixtures)
- FOUND: commit 28b906d9 (feat: HoldProvider interface + Options field)
- FOUND: commit f2df7549 (feat: markPhase HoldProvider wiring + legacy test delete)
- FOUND: commit 002ab678 (test: HoldProvider nil-safe / positive / fail-closed)
