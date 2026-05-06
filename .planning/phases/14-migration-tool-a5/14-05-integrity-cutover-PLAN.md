---
phase: 14-migration-tool-a5
plan: 05
type: execute
wave: 5
depends_on: [14-03-migrate-tool-core, 14-04-bandwidth-parallel]
files_modified:
  - pkg/blockstore/remote/remote.go
  - pkg/blockstore/remote/s3/store.go
  - pkg/blockstore/remote/memory/store.go
  - pkg/blockstore/remote/remotetest/suite.go
  - cmd/dfsctl/commands/blockstore/migrate_integrity.go
  - cmd/dfsctl/commands/blockstore/migrate_cutover.go
  - cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go
  - cmd/dfsctl/commands/blockstore/migrate_loop.go
  - cmd/dfsctl/commands/blockstore/migrate_integrity_test.go
  - cmd/dfsctl/commands/blockstore/migrate_cutover_test.go
  - cmd/dfsctl/commands/blockstore/migrate_legacy_gc_test.go
  - pkg/metadata/store.go
autonomous: true
requirements: [MIG-01, MIG-04]
tags: [integrity_check, cutover, legacy_gc, head_per_ref, fail_loud]
must_haves:
  truths:
    - "After the migration loop completes, the tool runs an integrity check: HEAD(cas/{hh}/{hh}/{hex}) for every unique ContentHash referenced by any migrated FileAttr.Blocks; assertion: 200 for every key (D-A12)"
    - "The integrity check also verifies the x-amz-meta-content-hash header on each HEAD response equals blake3:{hex} (D-A12 parity check)"
    - "On integrity-check pass, the tool flips the share's BlockLayout to cas-only AND deletes legacy {payloadID}/block-{idx} keys, in that order. The flip is one metadata txn; legacy delete iterates and continues on individual failures (best-effort; orphans are GC-eligible) (D-A7, D-A13)"
    - "On integrity-check fail, the tool exits non-zero, leaves BlockLayout=legacy, leaves journal in place, leaves any new CAS chunks in S3 (orphans → GC reclaims). No automatic legacy-key restore. (D-A8 fail-loud)"
    - "The auto-cutover txn: metadataStore.UpdateShareOptions sets BlockLayout=cas-only ONLY after integrity check passes. After this txn returns success, then-and-only-then does the tool iterate legacy keys and delete (D-A13)"
    - "RemoteStore.HeadObject(ctx, key) → (HeadResult, error) is on the public RemoteStore interface and implemented by every backend (s3, memory). HeadResult exposes ContentLength + Metadata map keyed lowercased so callers can read the content-hash header. (BLOCKER 1 fix — interface was missing in pre-revision plan.)"
  artifacts:
    - path: pkg/blockstore/remote/remote.go
      provides: "HeadObject method added to the RemoteStore interface; HeadResult type with ContentLength + Metadata map[string]string"
      contains: "HeadObject"
    - path: pkg/blockstore/remote/s3/store.go
      provides: "S3 backend implementation of HeadObject — uses AWS SDK HeadObject; returns the existing x-amz-meta-* metadata via the Metadata map"
      contains: "HeadObject"
    - path: pkg/blockstore/remote/memory/store.go
      provides: "Memory backend implementation of HeadObject — returns ContentLength + the metadata stamped at WriteBlockWithHash time"
      contains: "HeadObject"
    - path: pkg/blockstore/remote/remotetest/suite.go
      provides: "TestHeadObjectRoundTrip conformance scenario — asserts HEAD on an existing CAS chunk returns 200 + content-hash metadata; HEAD on missing key returns ErrBlockNotFound"
      contains: "HeadObjectRoundTrip"
    - path: cmd/dfsctl/commands/blockstore/migrate_integrity.go
      provides: "verifyIntegrity — collects unique hashes from migrated FileAttr.Blocks, HEADs each, asserts metadata header"
      contains: "verifyIntegrity"
    - path: cmd/dfsctl/commands/blockstore/migrate_cutover.go
      provides: "performCutover — UpdateShareOptions(BlockLayout=cas-only) atomically"
      contains: "BlockLayoutCASOnly"
    - path: cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go
      provides: "deleteLegacyKeys — iterates {payloadID}/block-{idx} and removes from remote store"
      contains: "FormatStoreKey"
  key_links:
    - from: cmd/dfsctl/commands/blockstore/migrate_integrity.go
      to: pkg/blockstore/remote/remote.go (HeadObject)
      via: "svc.RemoteHeadObject calls into rs.HeadObject (newly added)"
      pattern: "HeadObject"
    - from: cmd/dfsctl/commands/blockstore/migrate_loop.go
      to: cmd/dfsctl/commands/blockstore/migrate_integrity.go
      via: "post-loop sequential call (loop ends → integrity → cutover → legacy delete)"
      pattern: "verifyIntegrity"
    - from: cmd/dfsctl/commands/blockstore/migrate_cutover.go
      to: pkg/metadata/store.go (UpdateShareOptions)
      via: "metadata-store txn flipping BlockLayout"
      pattern: "UpdateShareOptions"
    - from: cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go
      to: pkg/blockstore/types.go (FormatStoreKey)
      via: "iterate legacy keys via remote store List + FormatStoreKey filter"
      pattern: "FormatStoreKey"
---

<objective>
Wire the post-migration integrity check (HEAD-per-ref + x-amz-meta-content-hash parity), the auto-cutover txn (`BlockLayout: legacy → cas-only`), and end-of-share legacy-key deletion. Tie them into the migration loop's tail so a successful run leaves the share fully cut over to CAS. Failures are fail-loud with no automatic rollback. (MIG-01 completion + MIG-04; D-A7, D-A8, D-A12, D-A13.)

Purpose: Plans 03–04 shipped the *re-chunk* part. Without this plan the operator would have to manually run `dfsctl share update --block-layout cas-only` and `aws s3 rm` — error-prone. D-A7 specifies one-step success: integrity passes → flag flips → legacy gone.

**BLOCKER 1 fix from review iteration 1:** the original plan called `svc.RemoteHeadObject()` and noted "HeadObject is missing on the remote interface, EXTEND it" but did not list the interface + backends + conformance suite in `files_modified` and did not gate the verify on cross-package compile. This revision pulls that work explicitly into Task 1 (Step 1 below) and broadens the verify to `go build ./...`.

Output: `dfsctl blockstore migrate --share NAME` is now end-to-end. Pre-flight: offline check. Loop: re-chunk. Post: integrity-check → cutover → legacy delete.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-03-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-04-SUMMARY.md
@.planning/codebase/CONVENTIONS.md

<interfaces>
<!-- Existing surfaces this plan consumes. -->

From pkg/metadata/store.go:
```go
// UpdateShareOptions: existing, single-share txn point. Plan 14-01 added
// BlockLayout to ShareOptions; flip via:
//   opts, _ := mds.GetShareOptions(ctx, share)
//   opts.BlockLayout = metadata.BlockLayoutCASOnly
//   mds.UpdateShareOptions(ctx, share, opts)
UpdateShareOptions(ctx context.Context, shareName string, options *ShareOptions) error
```

NEW surface added in this plan (BLOCKER 1 — was missing in pre-revision plan):

```go
// In pkg/blockstore/remote/remote.go:

// HeadObject returns object metadata without fetching the body. Used by
// the Phase 14 migration tool's post-migration integrity check
// (D-A12) — asserts (1) every CAS chunk referenced by migrated
// FileAttr.Blocks exists (200) and (2) the x-amz-meta-content-hash
// header equals blake3:{hex} of the key's hash component.
//
// Returns blockstore.ErrBlockNotFound when the object is missing.
HeadObject(ctx context.Context, key string) (HeadResult, error)

// HeadResult exposes content length and lowercased user-metadata
// headers (e.g. "content-hash" from S3's x-amz-meta-content-hash).
type HeadResult struct {
    ContentLength int64
    Metadata      map[string]string
}
```

Implementation notes:

- s3 backend: AWS SDK `HeadObject` returns the x-amz-meta-* headers in
  `output.Metadata map[string]string` — keys ARE lowercased by the SDK,
  so passing them straight through satisfies the contract.
- memory backend: pkg/blockstore/remote/memory/store.go already stamps
  the content-hash on WriteBlockWithHash (per remotetest assertions);
  HeadObject just exposes that stored map.
- remotetest/suite.go: add `TestHeadObjectRoundTrip(t, store)` —
  WriteBlockWithHash a known chunk, HeadObject the key, assert
  ContentLength and `Metadata["content-hash"] == "blake3:" + hex`.
  All three backends invoke this scenario from their existing test
  entry points (grep `TestRemoteStoreSuite` to find them).

From pkg/blockstore/types.go:
```go
func FormatStoreKey(payloadID string, blockIdx uint64) string  // legacy key
func FormatCASKey(h ContentHash) string                        // CAS key
```

From pkg/blockstore/remote/remote.go (existing):
```go
ListByPrefix(ctx context.Context, prefix string) ([]string, error)
ListByPrefixWithMeta(ctx context.Context, prefix string) ([]ObjectInfo, error)
DeleteBlock(ctx context.Context, blockKey string) error
```

`ListByPrefix` is the legacy-key enumerator the deleteLegacyKeys helper
uses (paired with `ParseStoreKey` filter).

From the migration loop (Plan 14-03):
```go
// Each migrateOneFile call returns blocks []BlockRef which are also written
// to the journal. The integrity check enumerates every unique hash from the
// post-migration FileAttr.Blocks across the share — i.e., re-walk the share
// after the loop is done and aggregate the union of FileAttr.Blocks[*].Hash.
// Use migrate.WalkShareFiles (Plan 14-03 Task 2) for the re-walk.
```
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Add HeadObject to RemoteStore + verifyIntegrity (HEAD-per-ref + content-hash header parity)</name>
  <files>
    pkg/blockstore/remote/remote.go,
    pkg/blockstore/remote/s3/store.go,
    pkg/blockstore/remote/memory/store.go,
    pkg/blockstore/remote/remotetest/suite.go,
    cmd/dfsctl/commands/blockstore/migrate_integrity.go,
    cmd/dfsctl/commands/blockstore/migrate_integrity_test.go
  </files>
  <read_first>
    - pkg/blockstore/remote/remote.go (current interface — verify HeadObject is NOT present; this task adds it)
    - pkg/blockstore/remote/s3/store.go (look at how WriteBlockWithHash + ReadBlockVerified currently invoke the AWS SDK — the SDK's `HeadObject` has the same auth + retry plumbing; mirror)
    - pkg/blockstore/remote/memory/store.go (look at how content-hash metadata is stored at WriteBlockWithHash time — HeadObject returns that same stored map)
    - pkg/blockstore/remote/remotetest/suite.go (existing conformance scenarios — find the entry function, e.g. `TestRemoteStoreSuite(t, factory)`, and add the new TestHeadObjectRoundTrip alongside)
    - pkg/blockstore/engine/syncer.go (look for the existing PUT path that emits x-amz-meta-content-hash — confirm header name and value format `blake3:{hex}`)
    - .planning/phases/14-migration-tool-a5/14-CONTEXT.md (D-A12 — HEAD per unique hash, parity check, linear in unique blocks)
  </read_first>
  <behavior>
    Interface + backends:
    - Test I1 (s3 + memory) — WriteBlockWithHash a 4 KiB chunk under key `cas/ab/cd/abcd...`; HeadObject the same key; assert `ContentLength == 4096` and `Metadata["content-hash"] == "blake3:" + hex`.
    - Test I2 (s3 + memory) — HeadObject on an unknown key returns `blockstore.ErrBlockNotFound` (or wraps it) — same convention as ReadBlock.

    verifyIntegrity:
    - Test V1: Empty share (no blocks) → verifyIntegrity returns nil; zero HEAD calls.
    - Test V2: Three files, 5 unique hashes after dedup → verifyIntegrity issues exactly 5 HEAD calls (proves the unique-set semantics — D-A12 "linear in unique-blocks").
    - Test V3: HEAD returns 404 for one hash → verifyIntegrity returns wrapped ErrIntegrityCheckFailed with the missing key in the error message.
    - Test V4: HEAD returns 200 but x-amz-meta-content-hash mismatches the expected blake3:{hex} → verifyIntegrity returns ErrIntegrityCheckFailed with header-mismatch detail.
    - Test V5: Network/transient error on HEAD bubbles up unwrapped (caller decides whether retry/fail-loud).
    - Test V6: Concurrent HEADs — verifyIntegrity uses a worker pool of size --parallel (default 4) so 1000 unique hashes don't take 1000 round-trips serially. Test asserts max in-flight == 4 with --parallel=4.
  </behavior>
  <action>
    **Step 1 — Add HeadObject to the RemoteStore interface (BLOCKER 1 fix).**

    Edit `pkg/blockstore/remote/remote.go`. Add the `HeadResult` type near the existing `ObjectInfo` struct, and the `HeadObject` method to the `RemoteStore` interface:

    ```go
    // HeadResult exposes object metadata returned by HeadObject. The
    // Metadata map is keyed in lowercase (S3 SDK normalizes; memory
    // backend follows the same convention) so callers can read
    // headers like "content-hash" without case juggling.
    type HeadResult struct {
        ContentLength int64
        Metadata      map[string]string
    }

    // (inside RemoteStore interface)

    // HeadObject returns object metadata without transferring the body.
    // Returns blockstore.ErrBlockNotFound (or a wrapping error) when
    // the key is missing.
    //
    // Used by the Phase 14 migration tool's post-migration integrity
    // check (D-A12): for every unique ContentHash referenced by any
    // migrated FileAttr.Blocks, HEAD the corresponding cas/.../h key
    // and assert (1) 200 and (2) Metadata["content-hash"] equals
    // "blake3:" + hex(hash).
    //
    // Implementations MUST surface user metadata stamped at
    // WriteBlockWithHash time (S3: x-amz-meta-* headers; memory:
    // in-process map). Header keys MUST be lowercased.
    HeadObject(ctx context.Context, key string) (HeadResult, error)
    ```

    **Step 2 — Implement on the S3 backend.** Edit `pkg/blockstore/remote/s3/store.go`. Use the AWS SDK `HeadObject` API; map 404 / NotFound to `blockstore.ErrBlockNotFound`; copy the SDK's `output.Metadata` map straight into HeadResult.Metadata (the SDK already lowercases). Apply the same `keyPrefix` handling existing methods use.

    **Step 3 — Implement on the memory backend.** Edit `pkg/blockstore/remote/memory/store.go`. Look up the key in the in-memory storage; if missing, return ErrBlockNotFound; otherwise return ContentLength + the stored metadata map (which WriteBlockWithHash already populates per the existing `x-amz-meta-content-hash presence (BSCAS-06)` comment).

    **Step 4 — Add conformance test.** Edit `pkg/blockstore/remote/remotetest/suite.go`. Add `TestHeadObjectRoundTrip(t *testing.T, store RemoteStore)` covering Tests I1 + I2 above. Wire it into the existing test entry function so all three backends (s3 via integration, memory via unit) execute it. Find the existing entry function by grepping `func Run` or `func TestRemoteStoreSuite` in the file.

    **Step 5 — Create the integrity helper.** Create `cmd/dfsctl/commands/blockstore/migrate_integrity.go`:

    ```go
    package blockstore

    import (
        "context"
        "errors"
        "fmt"
        "sync"
        "sync/atomic"

        "golang.org/x/sync/errgroup"

        "github.com/marmos91/dittofs/pkg/blockstore"
        "github.com/marmos91/dittofs/pkg/blockstore/migrate"
        "github.com/marmos91/dittofs/pkg/metadata"
    )

    var ErrIntegrityCheckFailed = errors.New("post-migration integrity check failed (MIG-04)")

    type integrityResult struct {
        UniqueHashes int
        HEADCalls    int
        Failures     []string
    }

    // verifyIntegrity walks the share's metadata, collects the union of
    // FileAttr.Blocks[*].Hash, and HEADs each unique CAS key. Asserts:
    //   1. Every key returns 200.
    //   2. Every response's "content-hash" metadata equals blake3:{hex(hash)}.
    // Concurrency: errgroup-bounded by opts.parallel.
    // D-A12.
    func verifyIntegrity(ctx context.Context, svc *offlineRuntime, opts migrateOptions) (integrityResult, error) {
        // 1. Walk metadata, collect unique hashes via the migrate package's
        //    walk helper (added in Plan 14-03 Task 2).
        uniq := map[blockstore.ContentHash]struct{}{}
        err := migrate.WalkShareFiles(ctx, svc.MetadataStore(), opts.share,
            func(handle metadata.FileHandle, file *metadata.File) error {
                for _, br := range file.Attr.Blocks {
                    uniq[br.Hash] = struct{}{}
                }
                return nil
            })
        if err != nil { return integrityResult{}, err }

        hashes := make([]blockstore.ContentHash, 0, len(uniq))
        for h := range uniq { hashes = append(hashes, h) }

        // 2. HEAD each unique key in parallel.
        var failuresMu sync.Mutex
        var failures []string
        var calls atomic.Int64

        g, gctx := errgroup.WithContext(ctx)
        parallel := opts.parallel
        if parallel < 1 { parallel = 4 }
        g.SetLimit(parallel)

        for _, h := range hashes {
            h := h
            g.Go(func() error {
                key := blockstore.FormatCASKey(h)
                res, err := svc.RemoteHeadObject(gctx, key)
                calls.Add(1)
                if err != nil {
                    if errors.Is(err, blockstore.ErrBlockNotFound) {
                        failuresMu.Lock()
                        failures = append(failures, fmt.Sprintf("%s: missing", key))
                        failuresMu.Unlock()
                        return nil
                    }
                    return err
                }
                want := h.String() // "blake3:{hex}"
                got := res.Metadata["content-hash"]
                if got != want {
                    failuresMu.Lock()
                    failures = append(failures, fmt.Sprintf("%s: header mismatch want=%q got=%q", key, want, got))
                    failuresMu.Unlock()
                }
                return nil
            })
        }
        if err := g.Wait(); err != nil { return integrityResult{}, err }

        result := integrityResult{
            UniqueHashes: len(hashes),
            HEADCalls:    int(calls.Load()),
            Failures:     failures,
        }
        if len(failures) > 0 {
            return result, fmt.Errorf("%w: %d/%d unique hashes failed; first: %s",
                ErrIntegrityCheckFailed, len(failures), len(hashes), failures[0])
        }
        return result, nil
    }
    ```

    **Step 6 — Tests.** Add `migrate_integrity_test.go` covering Tests V1–V6. Use a stub remote store (or the memory backend with controllable HEAD responses) to drive the assertions. For Test V6 concurrency assertion, use a `sync.WaitGroup`-style counter inside the stub.

    Conformance suite tests (I1, I2) live in `pkg/blockstore/remote/remotetest/suite.go` and run via the backend-specific test files (e.g. `pkg/blockstore/remote/memory/store_test.go`).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./pkg/blockstore/remote/... -run 'TestHeadObjectRoundTrip|TestRemoteStoreSuite' -count=1 &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestVerifyIntegrity' -count=1 &amp;&amp; go vet ./pkg/blockstore/remote/... ./cmd/dfsctl/commands/blockstore/ &amp;&amp; grep -c 'HeadObject' pkg/blockstore/remote/remote.go &amp;&amp; grep -q 'ErrIntegrityCheckFailed' cmd/dfsctl/commands/blockstore/migrate_integrity.go &amp;&amp; grep -q 'content-hash' cmd/dfsctl/commands/blockstore/migrate_integrity.go &amp;&amp; grep -q 'errgroup' cmd/dfsctl/commands/blockstore/migrate_integrity.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'HeadObject' pkg/blockstore/remote/remote.go` >= 2 (one in interface, one on HeadResult docstring or comment).
    - `grep -c 'HeadResult' pkg/blockstore/remote/remote.go` >= 1.
    - `grep -c 'func.*HeadObject' pkg/blockstore/remote/s3/store.go` >= 1.
    - `grep -c 'func.*HeadObject' pkg/blockstore/remote/memory/store.go` >= 1.
    - `grep -c 'HeadObjectRoundTrip' pkg/blockstore/remote/remotetest/suite.go` >= 1.
    - `grep -c 'ErrIntegrityCheckFailed' cmd/dfsctl/commands/blockstore/migrate_integrity.go` >= 1.
    - `grep -c 'FormatCASKey' cmd/dfsctl/commands/blockstore/migrate_integrity.go` >= 1.
    - `grep -c 'content-hash' cmd/dfsctl/commands/blockstore/migrate_integrity.go` >= 1.
    - `grep -c 'g.SetLimit' cmd/dfsctl/commands/blockstore/migrate_integrity.go` >= 1.
    - `grep -c 'migrate\.WalkShareFiles' cmd/dfsctl/commands/blockstore/migrate_integrity.go` >= 1 (re-uses Plan 14-03 walk helper).
    - All 6 verifyIntegrity unit tests + 2 backend conformance scenarios pass across ALL backends.
    - `go build ./...` succeeds (cross-package compile — catches the case where any RemoteStore implementer in the codebase is missing HeadObject; this is the test that would have caught BLOCKER 1 if it had been run).
  </acceptance_criteria>
  <done>
    HeadObject lands on the RemoteStore interface with implementations on s3 + memory backends and a conformance scenario covering all backends. Post-migration integrity check enumerates unique hashes, HEADs in parallel, asserts both existence and header parity, returns ErrIntegrityCheckFailed on any failure.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Auto-cutover (block_layout flip) + end-of-share legacy GC + wire into runMigrateLoop</name>
  <files>
    cmd/dfsctl/commands/blockstore/migrate_cutover.go,
    cmd/dfsctl/commands/blockstore/migrate_cutover_test.go,
    cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go,
    cmd/dfsctl/commands/blockstore/migrate_legacy_gc_test.go,
    cmd/dfsctl/commands/blockstore/migrate_loop.go
  </files>
  <read_first>
    - pkg/metadata/store.go (UpdateShareOptions signature)
    - pkg/metadata/types.go (BlockLayoutCASOnly constant from Plan 14-01)
    - pkg/blockstore/remote/remote.go (ListByPrefix is the existing list helper; ParseStoreKey filters legacy-only)
    - pkg/blockstore/types.go (FormatStoreKey + ParseStoreKey for filtering legacy-only keys)
    - cmd/dfsctl/commands/blockstore/migrate_loop.go (Plan 03 output — runMigrateLoop's tail; this is where Tasks 1+2 wire in)
  </read_first>
  <behavior>
    - Test 1 — performCutover happy: with a share whose BlockLayout=legacy in metadata, performCutover sets it to cas-only via UpdateShareOptions; subsequent GetShareOptions reflects cas-only.
    - Test 2 — performCutover idempotent: calling on a share already cas-only returns nil and does not error.
    - Test 3 — performCutover failure: UpdateShareOptions returns an error → performCutover returns the wrapped error; legacy keys MUST NOT be deleted (caller must short-circuit).
    - Test 4 — deleteLegacyKeys happy: 100 legacy keys exist in the remote store under various payloadIDs; after delete, ListByPrefix(... matching legacy pattern ...) returns 0 keys.
    - Test 5 — deleteLegacyKeys partial failure: a delete call returns an error for 1 of 100 keys → tool logs the failure but continues; final return is non-nil (aggregated). The orphaned legacy key is GC-eligible (mark-sweep won't see it in any live FileAttr.Blocks list, but legacy keys aren't on the cas/.../h prefix the GC scans — this is a separate consideration, document in the SUMMARY).
    - Test 6 — runMigrateLoop end-to-end: integrity passes → cutover → legacy delete → returns nil. Integrity fails → returns wrapped ErrIntegrityCheckFailed; cutover NOT called; legacy keys NOT deleted; share's BlockLayout still legacy.
    - Test 7 — runMigrateLoop dry-run skips integrity, cutover, AND legacy delete entirely. Reports a "would-cut-over" line in the human output.
  </behavior>
  <action>
    1. Create `cmd/dfsctl/commands/blockstore/migrate_cutover.go`:
       ```go
       package blockstore

       import (
           "context"
           "fmt"

           "github.com/marmos91/dittofs/internal/logger"
           "github.com/marmos91/dittofs/pkg/metadata"
       )

       // performCutover flips the share's BlockLayout from legacy to cas-only
       // via metadataStore.UpdateShareOptions. Idempotent (safe to call on an
       // already-cas-only share). D-A6, D-A7.
       func performCutover(ctx context.Context, svc *offlineRuntime, share string) error {
           opts, err := svc.MetadataStore().GetShareOptions(ctx, share)
           if err != nil { return fmt.Errorf("read share options: %w", err) }
           if opts.BlockLayout == metadata.BlockLayoutCASOnly {
               logger.Info("share already cas-only; cutover is a no-op", "share", share)
               return nil
           }
           opts.BlockLayout = metadata.BlockLayoutCASOnly
           if err := svc.MetadataStore().UpdateShareOptions(ctx, share, opts); err != nil {
               return fmt.Errorf("flip block_layout to cas-only for share %q: %w", share, err)
           }
           logger.Info("share block_layout flipped to cas-only", "share", share)
           return nil
       }
       ```

    2. Create `cmd/dfsctl/commands/blockstore/migrate_legacy_gc.go`:
       ```go
       package blockstore

       import (
           "context"
           "fmt"
           "strings"
           "sync"
           "sync/atomic"

           "golang.org/x/sync/errgroup"

           "github.com/marmos91/dittofs/internal/logger"
           "github.com/marmos91/dittofs/pkg/blockstore"
       )

       // deleteLegacyKeys enumerates {payloadID}/block-{idx} objects in the
       // share's remote store and deletes them. Best-effort: a per-key error
       // is logged but does not abort the sweep. Aggregate non-nil error
       // returned if any deletes failed (operator informs orphan-key cleanup).
       // D-A13.
       //
       // Filtering: ParseStoreKey identifies legacy keys; cas/... keys are
       // skipped. Uses the existing RemoteStore.ListByPrefix("") (or a per-
       // payload prefix list) — no new remote-store API needed.
       func deleteLegacyKeys(ctx context.Context, svc *offlineRuntime, opts migrateOptions) (int, error) {
           rs := svc.RemoteStore()
           keys, err := rs.ListByPrefix(ctx, "")
           if err != nil { return 0, fmt.Errorf("list remote keys: %w", err) }

           legacy := make([]string, 0, len(keys))
           for _, k := range keys {
               if strings.HasPrefix(k, "cas/") { continue }
               if _, _, ok := blockstore.ParseStoreKey(k); ok {
                   legacy = append(legacy, k)
               }
           }
           logger.Info("legacy keys identified for deletion", "share", opts.share, "count", len(legacy))

           // Parallel delete via errgroup; honors --parallel.
           parallel := opts.parallel
           if parallel < 1 { parallel = 4 }
           g, gctx := errgroup.WithContext(ctx)
           g.SetLimit(parallel)

           var failuresMu sync.Mutex
           var failures []string
           var deleted atomic.Int64

           for _, k := range legacy {
               k := k
               g.Go(func() error {
                   if err := rs.DeleteBlock(gctx, k); err != nil {
                       failuresMu.Lock()
                       failures = append(failures, fmt.Sprintf("%s: %v", k, err))
                       failuresMu.Unlock()
                       return nil // continue
                   }
                   deleted.Add(1)
                   return nil
               })
           }
           _ = g.Wait()

           if len(failures) > 0 {
               return int(deleted.Load()), fmt.Errorf("legacy delete: %d of %d keys failed; first: %s",
                   len(failures), len(legacy), failures[0])
           }
           return int(deleted.Load()), nil
       }
       ```

       Note: `ListByPrefix("")` returns every object the share's remote store sees. For TB-scale shares with millions of keys, the executor MAY refactor to per-payload-id list (iterate `migrate.WalkShareFiles` and call `rs.ListByPrefix(payloadID + "/")` per file) — document the choice in the SUMMARY.

    3. Wire into `runMigrateLoop` in `migrate_loop.go`:
       ```go
       // After workerPool.Run completes successfully...
       if !opts.dryRun {
           ir, err := verifyIntegrity(ctx, svc, opts)
           if err != nil {
               // Fail-loud per D-A8: leave block_layout=legacy, leave legacy keys, leave journal.
               logger.Error("integrity check failed; aborting cutover and legacy delete",
                   "share", opts.share, "unique_hashes", ir.UniqueHashes, "head_calls", ir.HEADCalls,
                   "failures", len(ir.Failures))
               return err
           }
           if err := performCutover(ctx, svc, opts.share); err != nil {
               return err
           }
           deletedCount, gcErr := deleteLegacyKeys(ctx, svc, opts)
           if gcErr != nil {
               logger.Warn("legacy key deletion had partial failures",
                   "share", opts.share, "deleted", deletedCount, "err", gcErr)
           }
           result.LegacyKeysDeleted = deletedCount
       } else {
           logger.Info("dry-run: would have run integrity check + cutover + legacy delete",
               "share", opts.share)
       }
       ```

    4. Tests:
       - `migrate_cutover_test.go` covers behaviors 1–3.
       - `migrate_legacy_gc_test.go` covers behaviors 4–5.
       - Extend `migrate_loop_test.go` with behaviors 6–7 (end-to-end happy + integrity-fail-aborts-cutover + dry-run-skips-everything).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestPerformCutover|TestDeleteLegacyKeys|TestMigrateLoop_EndToEnd|TestMigrateLoop_IntegrityFail|TestMigrateLoop_DryRunSkipsCutover' -count=1 -timeout 60s &amp;&amp; go vet ./cmd/dfsctl/commands/blockstore/ &amp;&amp; grep -q 'BlockLayoutCASOnly' cmd/dfsctl/commands/blockstore/migrate_cutover.go &amp;&amp; grep -q 'verifyIntegrity' cmd/dfsctl/commands/blockstore/migrate_loop.go &amp;&amp; grep -q 'deleteLegacyKeys' cmd/dfsctl/commands/blockstore/migrate_loop.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'verifyIntegrity' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'performCutover' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - `grep -c 'deleteLegacyKeys' cmd/dfsctl/commands/blockstore/migrate_loop.go` >= 1.
    - The order verifyIntegrity → performCutover → deleteLegacyKeys is preserved (Task 2's wiring snippet in `<action>` is the canonical sequence — early-return on integrity fail, no cutover; cutover error short-circuits before legacy delete).
    - `grep -c 'opts.dryRun' cmd/dfsctl/commands/blockstore/migrate_loop.go` (in the post-loop section) >= 1 (dry-run skips everything).
    - All listed tests pass.
    - `go build ./...` succeeds.
  </acceptance_criteria>
  <done>
    Migration loop is now end-to-end: re-chunk → integrity check → cutover → legacy delete; integrity failure aborts cutover; dry-run skips post-processing entirely; partial legacy-delete failures are best-effort (reported, not fatal).
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Migration tool → metadata store (cutover txn) | The flip is a single UpdateShareOptions txn. Concurrent operator action (e.g., `dfsctl share update --read-only true`) could interleave; the offline invariant (D-A5) plus single-process tool minimize this. |
| Migration tool → S3 DeleteObject | Best-effort sweep. Unauthorized DELETE is prevented by IAM at the S3 layer; the tool inherits the operator's credentials. |
| Migration tool → S3 HeadObject | Read-only metadata fetch. New surface in this plan; same auth + transport plumbing as ReadBlockVerified. No new threat surface. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-05-01 | Tampering | Integrity check passes for blocks that were tampered post-migration | mitigate | Header parity check (`x-amz-meta-content-hash` vs `blake3:{hex}` derived from the key path) catches tampering that updates the object but not the header, AND vice versa. Hash-of-key invariant means tampering with bytes will surface on the next ReadBlockVerified call. |
| T-14-05-02 | Tampering | Operator deletes legacy keys before integrity check passes | mitigate | Code path is strictly sequential: integrity check first; cutover only on success; legacy delete only after cutover succeeds. The early-return on integrity failure is asserted by Test 6. |
| T-14-05-03 | Information disclosure | Logging full ContentHash in failure messages | accept | Hashes are blake3 of public file contents, not secrets. Operator-visible failure logs are necessary for triage. |
| T-14-05-04 | DoS | Legacy delete enumerates a giant share's full key namespace | mitigate | Per-payload-id fallback list path (in deleteLegacyKeys) avoids a single huge ListByPrefix sweep on TB-scale shares; document the choice in the SUMMARY and runbook (Plan 07). |
| T-14-05-05 | Repudiation | A successful integrity check followed by an uncaught panic before cutover leaves the share in an inconsistent state | mitigate | All three steps are in the same process invocation. A panic is caught at the dfsctl root level and logs the partial-state. The operator re-runs; cutover is idempotent (Test 2 covers it); integrity check is also idempotent (HEAD is read-only). |
</threat_model>

<verification>
- All 7 unit + integration tests pass.
- HeadObject lands on RemoteStore + s3 + memory backends + conformance suite.
- Integrity check: HEAD per unique hash + header parity, parallel.
- Cutover: idempotent UpdateShareOptions txn.
- Legacy delete: best-effort with aggregated failure reporting.
- Pipeline: integrity → cutover → legacy delete; failures abort the right things.
- Dry-run skips all post-processing.
- `go build ./...`, `go vet ./...` clean (cross-package compile catches missing-method drift).
</verification>

<success_criteria>
- `dfsctl blockstore migrate --share NAME` end-to-end: offline check → re-chunk → integrity → cutover → legacy GC.
- ErrIntegrityCheckFailed surfaces on any HEAD failure or header mismatch.
- BlockLayout flips to cas-only only on integrity success.
- Legacy keys deleted only after cutover succeeds.
- Dry-run leaves no trace.
- Fail-loud preserves journal + legacy keys for re-run.
- HeadObject is on the public RemoteStore interface with conformance coverage.
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-05-SUMMARY.md` documenting the verifyIntegrity / performCutover / deleteLegacyKeys contract, the strict ordering invariant, and the new HeadObject interface surface (BLOCKER 1 fix). Note the orphaned-legacy-keys consideration (legacy GC is not part of mark-sweep — covered explicitly).
</output>
