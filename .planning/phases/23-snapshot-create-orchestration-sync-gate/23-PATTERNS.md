# Phase 23: Snapshot Create Orchestration + Sync Gate — Pattern Map

**Mapped:** 2026-05-28
**Files analyzed:** 2 new + 5 modified
**Analogs found:** 7 / 7 (all in-repo, exact role match)

---

## File Classification

| File | New / Modified | Role | Data Flow | Closest Analog | Match Quality |
|------|----------------|------|-----------|----------------|---------------|
| `pkg/snapshot/syncgate.go` | new | pure helper (probe / verify) | bounded-parallel request-response, fail-fast | `pkg/blockstore/engine/syncer.go::mirrorOnce` (per-hash remote loop) + `pkg/adapter/base.go` (channel semaphore pattern) | role-match (verify is read-only; mirror is write — same iteration shape) |
| `pkg/controlplane/runtime/snapshot.go` | new | orchestrator (Runtime method + async goroutine + registry) | event-driven async pipeline (backup → manifest → drain → verify → state-flip) | `pkg/controlplane/runtime/snapshot_hold.go` (same package, same Snapshot domain) + `pkg/controlplane/runtime/blockgc.go` (Runtime orchestration glue over engine surfaces) + `pkg/controlplane/runtime/shares/service.go` (lock-protected registry pattern) | exact (snapshot_hold) + role-match (blockgc, shares) |
| `pkg/controlplane/runtime/snapshot_hold.go` | modified | provider (filter revision + lock) | enumerate-and-stream | self (revise in place) — see `HeldHashes` body lines 31–74 | self |
| `pkg/controlplane/runtime/runtime.go` | modified | struct field + init + Shutdown | composition | `Runtime` struct (lines 52–81) + `New` (lines 83–105) + `Serve` (line 370) | self |
| `pkg/controlplane/runtime/shares/service.go::RemoveShare` | modified | lifecycle integration | sequential cleanup | self (lines 760–800) — insert cancel+Wait BEFORE the existing `snapshots/` wipe at line 781 | self |
| `pkg/controlplane/models/errors.go` | modified | error sentinels | n/a | self (lines 1–43) | self |
| `pkg/config/config.go` (or `pkg/config/blockstore.go`) | modified | config schema knob + default + validate | n/a | `GCConfig` (lines 132–205) — exact precedent for a `<domain>Config` block with `ApplyDefaults`+`Validate` | exact |

---

## Pattern Assignments

### `pkg/snapshot/syncgate.go` (new — pure helper, bounded-parallel probe)

**Role:** Pure helper. No Runtime fixtures. Mirrors the Phase 22 `manifest.go` placement: pure I/O against the `RemoteStore` + `*HashSet` arguments.

**Package boilerplate / imports** — model on `pkg/snapshot/manifest.go:1-12`:

```go
package snapshot

import (
    "context"
    "errors"
    "fmt"
    "sync"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/blockstore/remote"
)
```

**Sentinel-error pattern** — copy shape from `pkg/snapshot/manifest.go:14-18` (note Phase 23 sentinels for the Runtime layer live in `pkg/controlplane/models/errors.go`; if `syncgate.go` needs a *package-local* "missing hash" wrapper, use this same exported-`var` style):

```go
// ErrInvalidManifestLine is returned by ReadManifest when a line in the
// manifest cannot be parsed as a 32-byte hex ContentHash. Callers should
// match it with errors.Is. The wrapped error carries the offending line
// number and the underlying parse error.
var ErrInvalidManifestLine = errors.New("snapshot: invalid manifest line")
```

**Iterate-`HashSet`-and-call-per-hash pattern** — model on `pkg/controlplane/runtime/snapshot_hold.go:78-93` (`streamManifest` → `hs.ForEach(fn)`):

```go
hs, err := snapshot.ReadManifest(f)
if err != nil {
    return 0, err
}
if err := hs.ForEach(fn); err != nil {
    return 0, err
}
return hs.Len(), nil
```

For `VerifyRemoteDurability` the equivalent is: iterate `manifest.Sorted()` (deterministic; matches `WriteManifest` line 31) and dispatch each into the worker pool.

**Bounded-parallel + fail-fast cancellation pattern** — model on `pkg/adapter/base.go:247-256` (buffered-channel semaphore + select on cancellation):

```go
// Acquire connection semaphore if connection limiting is enabled
if b.connSemaphore != nil {
    select {
    case b.connSemaphore <- struct{}{}:
        // Acquired semaphore slot, proceed with accept
    case <-b.Shutdown:
        // Shutdown initiated while waiting for semaphore
        return b.gracefulShutdown()
    }
}
```

Apply this shape in `VerifyRemoteDurability`: a `chan struct{}` of capacity `concurrency` gates dispatch, and `<-ctx.Done()` is the fail-fast trigger. Use a derived `errCtx, cancel := context.WithCancel(ctx)` so the first miss cancels siblings.

**`Head()`-vs-`ErrBlockNotFound` discrimination** — model on the `RemoteStore.Head` contract at `pkg/blockstore/remote/remote.go:74-88` and the sentinel at `pkg/blockstore/errors.go:121-123`:

```go
// blockstore/errors.go
ErrBlockNotFound = errors.New("block not found")
```

Verify body shape: `_, err := remote.Head(ctx, hash); switch { case err == nil: /* present, continue */ ; case errors.Is(err, blockstore.ErrBlockNotFound): /* miss → cancel siblings, record hash */ ; default: /* I/O error → fail-fast */ }`.

**Signature anchor (per CONTEXT D-23-06 / D-23-07 / D-23-08):**

```go
func VerifyRemoteDurability(
    ctx context.Context,
    rs remote.RemoteStore,
    manifest *blockstore.HashSet,
    concurrency int,
) error
```

Return `fmt.Errorf("snapshot: remote durability verify: missing hash %s: %w", hash, blockstore.ErrBlockNotFound)` — Phase 23 D-23-12 sentinel wrapping happens *one layer up* in `runtime/snapshot.go` (the Runtime caller wraps with `ErrSnapshotVerifyFailed`).

**Co-located test pattern** — model placement on `pkg/snapshot/manifest_test.go` (referenced by Phase 22 VERIFICATION 11 tests passing). Use `remote/memory.Store` (see `pkg/controlplane/runtime/snapshot_lifecycle_test.go:87-92` for the memory-remote construction pattern).

---

### `pkg/controlplane/runtime/snapshot.go` (new — orchestrator, async pipeline)

**Role:** Runtime methods (`CreateSnapshot`, `WaitForSnapshot`, startup recovery, registry helpers). Per CONTEXT D-23-14, this lives ON `*Runtime` as methods, NOT a new sub-service. Per D-23-21, pure helpers (dump-writer, retry-validator) extracted into `pkg/snapshot/`.

**Package + imports** — model on `pkg/controlplane/runtime/snapshot_hold.go:1-15` (same package, same Snapshot domain, all the imports Phase 23 needs are already there):

```go
package runtime

import (
    "context"
    "errors"
    "fmt"
    "os"

    "github.com/marmos91/dittofs/internal/logger"
    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/blockstore/engine"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
    "github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
    "github.com/marmos91/dittofs/pkg/snapshot"
)
```

Phase 23 will additionally import `sync` (for the registry mutex + WaitGroup), `time`, `github.com/google/uuid` (for fresh snap IDs).

**Runtime-method-with-nil-safety pattern** — exact precedent at `pkg/controlplane/runtime/snapshot_hold.go:31-34`:

```go
func (p *SnapshotHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, _ []string, fn func(blockstore.ContentHash) error) error {
    if p == nil || p.rt == nil || p.rt.store == nil {
        return nil
    }
```

`CreateSnapshot` similarly guards `r == nil || r.store == nil` at the top.

**Resolve-per-share-store + ErrShareNotFound pattern** — model on `snapshot_hold.go:36-48`:

```go
for _, shareName := range p.shares {
    localStoreDir, err := p.rt.sharesSvc.LocalStoreDir(shareName)
    if err != nil {
        // Share removed between GC entry and hold enumeration —
        // no held hashes to contribute.
        if errors.Is(err, shares.ErrShareNotFound) {
            continue
        }
        return fmt.Errorf("snapshot hold: resolve local store dir for share %q: %w", shareName, err)
    }
    if localStoreDir == "" {
        continue
    }
```

`CreateSnapshot` derives `localStoreDir` the same way; an empty string means memory backend — return an error (`CreateSnapshot` cannot run against memory because there's nowhere to write the dump+manifest).

**Type-assert-`Backupable`-with-nil-safe-fallback pattern** — `pkg/metadata/backupable.go:16-21`:

```go
// Call sites discover the capability via a type assertion:
//
//	if b, ok := store.(metadata.Backupable); ok {
//	    hashes, err := b.Backup(ctx, w)
//	    ...
//	}
```

In `CreateSnapshot`:
```go
metaStore, err := r.GetMetadataStoreForShare(shareName)
if err != nil { /* wrap with ErrSnapshotBackupFailed */ }
backupable, ok := metaStore.(metadata.Backupable)
if !ok { return "", fmt.Errorf("metadata engine for share %q does not support backup: %w", shareName, ErrSnapshotBackupFailed) }
```

**Per-share lock-protected registry pattern** — model on `pkg/controlplane/runtime/shares/service.go:760-800`:

```go
func (s *Service) RemoveShare(name string) error {
    s.mu.Lock()
    share, exists := s.registry[name]
    if !exists {
        s.mu.Unlock()
        return fmt.Errorf("share %q not found", name)
    }
    bs := share.BlockStore
    remoteConfigID := share.remoteConfigID
    localStoreDir := share.localStoreDir
    delete(s.registry, name)
    s.mu.Unlock()
```

Phase 23 mirrors this: lock the registry briefly to fetch the `*snapInFlight` entry, snapshot what we need, release, then do the slow `wg.Wait()` outside the lock.

**`snapInFlight` shape (CONTEXT D-23-17 / D-23-19)** — assemble from these existing snippets:
- `sync.WaitGroup` + `sync.RWMutex` field placement: `Syncer` struct at `pkg/blockstore/engine/syncer.go:69-89` (closed/mu/stopCh fields)
- Stop-channel pattern (per-snap completion signal for `WaitForSnapshot`): `Syncer.stopCh` at `syncer.go:79`, used via `close(m.stopCh)` at `syncer.go:788`. For Phase 23: one `chan struct{}` per snap, closed when the orchestration goroutine exits.

**Orchestration-step + slog instrumentation pattern** — model on `pkg/controlplane/runtime/blockgc.go:66-89`:

```go
logger.Info("RunBlockGC: starting",
    "configID", entry.ConfigID,
    "shares", entry.Shares,
    "dryRun", dryRun,
    "gracePeriod", opts.GracePeriod,
    ...)
...
logger.Info("RunBlockGC: complete",
    "configID", entry.ConfigID,
    "hashesMarked", s.HashesMarked,
    "objectsSwept", s.ObjectsSwept,
    "bytesFreed", s.BytesFreed,
    "errors", s.ErrorCount,
)
```

Phase 23 keys (per CONTEXT D-23-16): `snapshot_id`, `share`, `bytes_dumped`, `manifest_count`, `drain_blocks`, `verify_concurrency`, `final_state`. Use `logger.Debug` on step-entry, `logger.Info` on step-completion. Use the same `slog`-style key/value pairs already standard across the runtime package.

**Calling `WriteManifestAtomic`** — model on its existing signature at `pkg/snapshot/manifest.go:49`:

```go
func WriteManifestAtomic(path string, hs *blockstore.HashSet) error
```

In `CreateSnapshot`: `manifestPath := snap.ManifestPath(localStoreDir); if err := snapshot.WriteManifestAtomic(manifestPath, hashSet); err != nil { /* wrap */ }`. The temp-file + fsync + rename is already inside, so the caller just supplies the path.

**Calling `Syncer.DrainAllUploads`** — exact signature at `pkg/blockstore/engine/syncer.go:388-393`:

```go
func (m *Syncer) DrainAllUploads(ctx context.Context) error {
    if err := m.SyncNow(ctx); err != nil {
        return err
    }
    return ctx.Err()
}
```

In `CreateSnapshot`: resolve `bs := r.sharesSvc.GetBlockStoreForShare(shareName)` (same pattern as `runtime.go:350`), then `bs.Syncer().DrainAllUploads(ctx)`. The `Syncer()` accessor is the public seam — check `engine.BlockStore` for the exposed method (Phase 22 VERIFICATION line 167 confirms `DrainAllUploads` is the documented API surface).

**State-flip-with-conditional-prior pattern (D-23-03 ready-flip)** — model on `pkg/controlplane/store/snapshots.go:88-117`:

```go
func (s *GORMStore) UpdateSnapshotState(ctx context.Context, shareName, id, state string) error {
    priors := allowedPriorStates(state)
    if len(priors) == 0 {
        return models.ErrSnapshotStateConflict
    }
    db := s.db.WithContext(ctx)
    res := db.Model(&models.Snapshot{}).
        Where("share_name = ? AND id = ? AND state IN ?", shareName, id, priors).
        Updates(map[string]any{...})
```

`UpdateSnapshotState` already exists; `CreateSnapshot` calls it with `state="ready"` for the success path and `state="failed"` for the failure paths. For D-23-03's `RemoteDurable=true` bundled write: the planner picks between (a) adding a new `UpdateSnapshotDurable(id, durable bool)` method (mentioned in CONTEXT line 146) or (b) extending `UpdateSnapshotState` to take an `extras map[string]any` overlay. Option (a) is simpler; either is acceptable.

**Concurrent-create conflict surfaced via partial unique index (D-23-13)** — model on `store/snapshots.go:28-34`:

```go
if err := s.db.WithContext(ctx).Create(snap).Error; err != nil {
    if isUniqueConstraintError(err) && snap.State == models.StateCreating {
        return "", models.ErrSnapshotStateConflict
    }
    return "", err
}
```

`CreateSnapshot` orchestration just propagates `ErrSnapshotStateConflict` to the caller without retry.

**Goroutine launched off a Runtime method, deferred cleanup pattern** — model on `pkg/blockstore/engine/syncer.go:580-636` (`Start` → `startPeriodicUploader` → `go m.periodicUploader(ctx, interval)`):

```go
go m.periodicUploader(ctx, interval)
```

Where `periodicUploader` at line 738 uses:
```go
for {
    select {
    case <-ticker.C:
        ...
    case <-m.stopCh:
        logger.Info("Periodic syncer: stopCh received, exiting")
        return
    case <-ctx.Done():
        logger.Info("Periodic syncer: context cancelled, exiting")
        return
    }
}
```

Phase 23 goroutine: simpler — runs once, not in a loop. `defer wg.Done()` + `defer close(completionCh)`. The cancellable child `ctx` comes from `context.WithCancel(runtimeCtx)` where `runtimeCtx` is captured at `New` (or `Serve`) time and cancelled on `Shutdown` (D-23-17).

**Centralized factory-method pattern for the registry helpers** — model on `pkg/controlplane/runtime/snapshot_hold.go:98-103`:

```go
func (r *Runtime) snapshotHoldForRemote(shareNames []string) engine.HoldProvider {
    return &SnapshotHoldProvider{
        rt:     r,
        shares: append([]string(nil), shareNames...),
    }
}
```

Phase 23: `(r *Runtime) registerSnapInFlight(shareName, snapID string) (childCtx, cancel, *snapInFlight)` follows the same shape — small private helper that wires up the map entry under lock.

---

### `pkg/controlplane/runtime/snapshot_hold.go` (modified — filter revision + RWMutex)

**Modification 1: D-23-02 filter** — current filter at line 56:

```go
for _, snap := range snaps {
    if snap.State != models.StateReady {
        continue
    }
```

Replace with the "manifest-on-disk = held" semantic. Per CONTEXT planner discretion (line 124): FS-walk is simpler (no DB schema change needed). Drop the `State != StateReady` filter; instead, per snapshot: stat the manifest path:

```go
for _, snap := range snaps {
    manifestPath := snap.ManifestPath(localStoreDir)
    if _, err := os.Stat(manifestPath); err != nil {
        if os.IsNotExist(err) {
            continue // no manifest = no hold
        }
        return fmt.Errorf("snapshot hold: stat manifest %q: %w", manifestPath, err)
    }
    // proceed to streamManifest as before
}
```

**Modification 2: D-23-04 per-snapshot RWMutex** — current `SnapshotHoldProvider` at lines 23–26:

```go
type SnapshotHoldProvider struct {
    rt     *Runtime
    shares []string
}
```

Add a lock manager. CONTEXT (D-23-04) leaves provider-level vs per-ID to planner discretion. For provider-level (simpler):

```go
type SnapshotHoldProvider struct {
    rt     *Runtime
    shares []string
    mu     sync.RWMutex // provider-wide: HeldHashes takes RLock; Delete takes Lock
}
```

Then `HeldHashes` body wraps the per-share loop in `p.mu.RLock(); defer p.mu.RUnlock()`. The `Delete` half lives on a new entrypoint Phase 23 introduces (the orchestration layer's snapshot-delete path), which acquires `p.mu.Lock()` before invoking `store.DeleteSnapshot` + `os.RemoveAll`. Race regression test pattern: model on the existing `TestSnapshotLifecycleVsGC` (`snapshot_lifecycle_test.go:137-279`) — add a sub-test using two goroutines hammering `HeldHashes` and `Delete` concurrently under `go test -race`.

---

### `pkg/controlplane/runtime/runtime.go` (modified — registry field + init + Shutdown)

**Field addition** — extend the existing `Runtime` struct at lines 52–81. Place after `clientRegistry` (lines 64) to keep related lifecycle bookkeeping together:

```go
type Runtime struct {
    mu    sync.RWMutex
    store store.Store
    ...
    clientRegistry *ClientRegistry

    // snapInFlight tracks per-share in-flight snapshot goroutines so
    // RemoveShare/Shutdown can cancel + wait before tearing down state.
    // Keyed by share name. See pkg/controlplane/runtime/snapshot.go.
    snapInFlight   map[string]*snapInFlight
    snapInFlightMu sync.Mutex
    ...
}
```

The `adapterProviders` field at lines 71–72 is the closest precedent for a "map keyed by name + dedicated mutex" pattern — same shape applies here.

**Init wiring (D-23-18)** — extend `New` at lines 83–105:

```go
func New(s store.Store) *Runtime {
    rt := &Runtime{
        store:            s,
        ...
        adapterProviders: make(map[string]any),
        snapInFlight:     make(map[string]*snapInFlight), // ADD
        ...
    }
    ...
    return rt
}
```

Startup recovery (D-23-18 — flip orphaned `creating` rows) runs from `Serve` (line 370), before adapters start. Precedent: `Syncer.recoverStaleSyncing` at `pkg/blockstore/engine/syncer.go:677-724` — same "scan-on-startup, flip rows, log on per-row failure, return aggregated error" shape:

```go
func (m *Syncer) recoverStaleSyncing(ctx context.Context) error {
    ...
    candidates, err := enum.EnumerateSyncingBlocks(ctx)
    if err != nil {
        return fmt.Errorf("enumerate syncing blocks: %w", err)
    }
    ...
    for _, fb := range candidates {
        ...
        fb.State = blockstore.BlockStatePending
        if err := m.fileBlockStore.Put(ctx, fb); err != nil {
            logger.Error("janitor: requeue failed", "blockID", fb.ID, "error", err)
            failed++
            if firstErr == nil { firstErr = err }
            continue
        }
        requeued++
    }
    ...
}
```

Phase 23 equivalent: `recoverOrphanedSnapshots(ctx)` — list every snap with `state='creating'` (the existing `ListSnapshots(ctx, shareName)` filters per-share, so this needs either a new `ListSnapshotsByState` or an iteration across `ListShares()` × `ListSnapshots`), flip to `failed` via `UpdateSnapshotState`. Phase 22 already has `UpdateSnapshotState` and the `failed` transition is allowed from `creating` (see `store/snapshots.go:121-128`).

**Shutdown wiring (D-23-17)** — `Serve` at line 370 delegates to `lifecycleSvc.Serve`. The Runtime needs a `Shutdown` hook that cancels every entry in `snapInFlight` and waits. Pattern from `Syncer.Close` at `pkg/blockstore/engine/syncer.go:779-807`:

```go
func (m *Syncer) Close() error {
    m.mu.Lock()
    if m.closed {
        m.mu.Unlock()
        return nil
    }
    m.closed = true
    m.mu.Unlock()
    close(m.stopCh)
    ...
    // Wait for in-flight uploads and flushes to complete
    ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
    defer cancel()
    _ = m.DrainAllUploads(ctx)
    ...
}
```

Apply the same shape: lock `snapInFlightMu` briefly to snapshot the cancel-funcs + waitgroups, release, then `cancel()` each and `wg.Wait()` each outside the lock with `DefaultShutdownTimeout` (line 23). Whether this lives as a new `(r *Runtime) shutdownSnapshots()` or piggybacks on `lifecycleSvc.Serve`'s shutdown path is planner discretion; the former keeps the snapshot domain self-contained.

---

### `pkg/controlplane/runtime/shares/service.go::RemoveShare` (modified — cancel+wait before wipe)

**Exact existing code at lines 762–800.** Phase 23 inserts the cancel+wait step BETWEEN the registry-delete (line 772) and the snapshots-dir wipe (line 781).

Current shape:
```go
func (s *Service) RemoveShare(name string) error {
    s.mu.Lock()
    share, exists := s.registry[name]
    ...
    delete(s.registry, name)
    s.mu.Unlock()

    // Cleanup per-share snapshot directories alongside registry removal.
    if localStoreDir != "" {
        snapsDir := filepath.Join(localStoreDir, "snapshots")
        if err := os.RemoveAll(snapsDir); err != nil {
            ...
        }
    }
    ...
}
```

The integration needs a Runtime-level callback because `shares.Service` does not hold a back-reference to `Runtime` (it predates the snap registry). Two acceptable approaches, planner picks:

1. **Callback registration** — `shares.Service` exposes a `SetRemoveShareHook(func(name string))` that the Runtime wires from `New`. The hook is invoked synchronously between `delete(s.registry, name)` and the snapshots-dir wipe. Matches the `OnShareChange` callback shape already at lines 322 / `service.go::notifyShareChange`.

2. **Move the integration up** — extend `Runtime.RemoveShare` (at `runtime.go:283-285`) to do the cancel+wait BEFORE delegating to `sharesSvc.RemoveShare`. This is the simpler option because the registry already lives on `*Runtime`.

Option (2) is preferred (D-23-17 already says "centralized registry on Runtime" — keeping the integration on Runtime preserves cohesion):

```go
func (r *Runtime) RemoveShare(name string) error {
    // Phase 23 D-23-17: cancel + wait for any in-flight snap goroutines
    // BEFORE the snapshots-tree wipe inside sharesSvc.RemoveShare.
    r.cancelAndWaitInFlightSnaps(name)
    return r.sharesSvc.RemoveShare(name)
}
```

---

### `pkg/controlplane/models/errors.go` (modified — five sentinels per D-23-12)

**Exact current contents at lines 1–43.** Phase 23 adds five sentinels to the existing `// Snapshot errors` block (lines 29–31):

```go
// Snapshot errors
ErrSnapshotNotFound      = errors.New("snapshot not found")
ErrSnapshotStateConflict = errors.New("snapshot is not in a state that allows this operation")

// Phase 23 (D-23-12): orchestration sentinels surfaced to REST in Phase 25.
ErrSnapshotBackupFailed         = errors.New("snapshot backup failed")
ErrSnapshotVerifyFailed         = errors.New("snapshot verify failed: missing hashes on remote after drain")
ErrSnapshotDrainTimeout         = errors.New("snapshot drain timed out")
ErrSnapshotRetryTargetNotFound  = errors.New("snapshot retry target not found")
ErrSnapshotRetryTargetNotFailed = errors.New("snapshot retry target is not in failed state")
```

Naming style match: `ErrSnapshotNotFound` / `ErrSnapshotStateConflict` from Phase 22 = `ErrSnapshot{Subject}{Condition}` — same convention applied. Naming match (CONTEXT discretion note: "match whichever style Phase 22 settled on for `ErrSnapshotNotFound`").

Co-located `errors.Is` round-trip test belongs in `pkg/controlplane/models/errors_test.go` (Phase 22 may or may not have created the file — if missing, create it; matches the test density seen on `pkg/snapshot/manifest_test.go`).

---

### Config schema (`pkg/config/config.go`) — modified (knob + default + validate)

**Exact precedent: `GCConfig` at lines 132–205.** Pattern is `<Domain>Config struct → ApplyDefaults → Validate`, all three named verbatim.

Two acceptable placements, planner picks:

1. **New `SnapshotConfig` block** — symmetric to `GCConfig`. Add field on the top-level `Config` struct (lines 41–82):

```go
// Snapshot configures the snapshot orchestration sync gate (Phase 23).
Snapshot SnapshotConfig `mapstructure:"snapshot" yaml:"snapshot"`
```

Then define alongside `GCConfig`:

```go
// SnapshotConfig configures the snapshot create orchestration (Phase 23).
// Knobs apply to every Runtime.CreateSnapshot invocation.
type SnapshotConfig struct {
    // SyncGateConcurrency bounds the parallel Head() probes the sync
    // gate fires against the remote during VerifyRemoteDurability.
    // Default: 16.
    SyncGateConcurrency int `mapstructure:"sync_gate_concurrency" yaml:"sync_gate_concurrency"`
}

func (c *SnapshotConfig) ApplyDefaults() {
    if c.SyncGateConcurrency <= 0 {
        c.SyncGateConcurrency = 16
    }
}

func (c *SnapshotConfig) Validate() error {
    if c.SyncGateConcurrency < 1 || c.SyncGateConcurrency > 256 {
        return fmt.Errorf("snapshot.sync_gate_concurrency must be in [1, 256] (got %d)", c.SyncGateConcurrency)
    }
    return nil
}
```

Default-value style match: `GCConfig.ApplyDefaults` at lines 162–173 uses `if c.X <= 0 { c.X = default }`. Validate style match: `GCConfig.Validate` at lines 184–205 uses `fmt.Errorf("gc.X must be ... (got %v)", c.X)`.

Wiring to runtime: extend `Runtime` with a `SetSnapshotDefaults` method mirroring `SetGCDefaults` at `runtime.go:487-491` and `gcDefaultsSnapshot` at lines 495–503. Then `CreateSnapshot` reads the snapshot of defaults under the runtime RLock at goroutine launch time.

2. **Single-knob extension of `GCConfig`** — adds `SyncGateConcurrency` to `GCConfig`. Simpler but couples snapshot tuning to GC; less clean. Avoid unless the planner determines `SnapshotConfig` overshoots.

---

## Shared Patterns

### Logger keys (slog style)

**Source:** `pkg/controlplane/runtime/blockgc.go:66-89` + `pkg/controlplane/runtime/snapshot_hold.go:65-70`
**Apply to:** Every orchestration log line in `runtime/snapshot.go` + every step inside the orchestration goroutine

```go
logger.Debug("snapshot hold: streamed hashes",
    "share", shareName,
    "snapshot_id", snap.ID,
    "count", count,
    "remote_endpoint_id", remoteEndpointID,
)
```

Phase 23 mandatory keys: `snapshot_id`, `share`. Phase 23 step-specific keys: `bytes_dumped` (after Backup), `manifest_count` (after WriteManifestAtomic), `drain_blocks` (after DrainAllUploads — if exposed; otherwise omit), `verify_concurrency`, `final_state`.

### Error wrapping with `%w` + sentinel

**Source:** `pkg/snapshot/manifest.go:33-41`, `pkg/controlplane/runtime/snapshot_hold.go:43-63`
**Apply to:** Every error return in `syncgate.go` + `runtime/snapshot.go`

```go
return fmt.Errorf("snapshot hold: stream manifest for share %q snapshot %q at %q: %w",
    shareName, snap.ID, manifestPath, err)
```

Phase 23 sentinel wrap pattern: `return fmt.Errorf("create snapshot %s: %w", snapID, ErrSnapshotBackupFailed)` — the underlying `Backupable.Backup` error chains via `errors.Join` if the planner wants both the sentinel AND the cause exposed. The standard convention in this codebase is single-`%w` wrap (single-cause), with the sentinel at the outer layer.

### Lock-protected registry with brief-hold pattern

**Source:** `pkg/controlplane/runtime/shares/service.go:763-773` + `pkg/controlplane/runtime/runtime.go:582-592` (`SetAdapterProvider` / `GetAdapterProvider`)
**Apply to:** Every `snapInFlight` read/write

```go
r.snapInFlightMu.Lock()
entry, ok := r.snapInFlight[shareName]
... // mutate / read short-lived state
r.snapInFlightMu.Unlock()
```

NEVER `wg.Wait()` while holding the registry lock — copy out the WG + cancels under lock, release, then wait.

### Memory-backend integration test fixture

**Source:** `pkg/controlplane/runtime/snapshot_lifecycle_test.go:28-119` (`lifecycleFixture`)
**Apply to:** Phase 23 P23-06 integration test (per CONTEXT plan D-23-20)

The fixture pattern provides: in-memory SQLite control plane (`cpstore.New`), memory metadata store, `setShareRemoteForTest` for the memory `RemoteStore`, `SetLocalStoreDirForTesting` for the local-store dir. Phase 23 reuses this verbatim and adds: an artificially-slow `Backupable` fixture (wrap `memorymetadata.NewMemoryMetadataStoreWithDefaults()` with a `Backup` that blocks on a channel) for the "RemoveShare cancels in-flight" sub-test.

---

## No Analog Found

| File / Concern | Reason |
|------|--------|
| (none) | Every Phase 23 file has a direct or near analog already in-tree. The "async orchestrator on Runtime" role has no perfect prior in this repo (block GC and syncer both run async but neither has the per-entity registry + WaitForX shape Phase 23 needs), but the component patterns (registry, child-ctx, WG, completion-chan) all have separate precedents above that compose cleanly. |

---

## Metadata

**Analog search scope:**
- `pkg/snapshot/`
- `pkg/controlplane/runtime/`
- `pkg/controlplane/runtime/shares/`
- `pkg/controlplane/models/`
- `pkg/controlplane/store/`
- `pkg/blockstore/engine/`
- `pkg/blockstore/remote/`
- `pkg/metadata/`
- `pkg/config/`
- `pkg/adapter/` (semaphore pattern only)

**Files scanned:** ~30 (targeted Reads + Greps; no full-tree walk)

**Pattern extraction date:** 2026-05-28

**Anchor reference files for executor pre-reads (must-load):**
- `pkg/snapshot/manifest.go`
- `pkg/controlplane/runtime/snapshot_hold.go`
- `pkg/controlplane/runtime/blockgc.go`
- `pkg/controlplane/runtime/runtime.go` (struct + init + Shutdown shape)
- `pkg/controlplane/runtime/shares/service.go` lines 760–800 (`RemoveShare`)
- `pkg/controlplane/runtime/snapshot_lifecycle_test.go` (integration test fixture)
- `pkg/controlplane/store/snapshots.go` (CRUD + state-flip)
- `pkg/controlplane/models/snapshot.go` (path helpers)
- `pkg/controlplane/models/errors.go` (sentinel style)
- `pkg/blockstore/engine/syncer.go` lines 380–393 (`DrainAllUploads`), 580–636 (Start), 677–724 (`recoverStaleSyncing`), 779–807 (`Close`), 738–776 (`periodicUploader`)
- `pkg/blockstore/remote/remote.go` (Head + ErrBlockNotFound contract)
- `pkg/blockstore/errors.go` (sentinel)
- `pkg/metadata/backupable.go` (interface + type-assert pattern)
- `pkg/config/config.go` lines 132–205 (`GCConfig` template)
- `pkg/adapter/base.go` lines 247–256 (channel-semaphore pattern)
