---
phase: 14-migration-tool-a5
plan: 02
type: execute
wave: 2
depends_on: [14-01-share-blocklayout]
files_modified:
  - pkg/blockstore/engine/engine.go
  - pkg/blockstore/engine/fetch.go
  - pkg/blockstore/engine/engine_dualread_test.go
  - pkg/controlplane/runtime/shares/service.go
autonomous: true
requirements: [MIG-03]
tags: [engine, dualread, block_layout, routing]
must_haves:
  truths:
    - "When a share's BlockLayout is cas-only, engine reads NEVER fall back to the legacy {payloadID}/block-{idx} path — they are served exclusively from cas/{hh}/{hh}/{hex} keys"
    - "When a share's BlockLayout is legacy (default for v0.13/v0.14 imports), the existing dual-read shim is unchanged: CAS path tried first via fb.Hash, legacy path tried as fallback"
    - "The BlockLayout flag is read once at share open (when the engine.BlockStore is created) and cached on the engine; runtime flips during the migration tool's auto-cutover txn require a share reload (handled in Plan 05)"
    - "An attempt to read a legacy key on a cas-only share surfaces as ErrLegacyReadOnCASOnly and is logged at Error (not silently zeroed) — fail-loud per D-A8"
  artifacts:
    - path: pkg/blockstore/engine/engine.go
      provides: "BlockStore.blockLayout field + Open-time wiring (read from ShareOptions)"
      contains: "blockLayout"
    - path: pkg/blockstore/engine/fetch.go
      provides: "Per-share gate inside dispatchRemoteFetch — refuse legacy fallback when blockLayout==cas-only"
      contains: "ErrLegacyReadOnCASOnly"
    - path: pkg/controlplane/runtime/shares/service.go
      provides: "createBlockStoreForShare passes ShareOptions.BlockLayout into the engine constructor"
      contains: "BlockLayout"
    - path: pkg/blockstore/engine/engine_dualread_test.go
      provides: "RED→GREEN tests asserting the per-share gate"
      contains: "TestDualRead_CASOnly_RefusesLegacyFallback"
  key_links:
    - from: pkg/controlplane/runtime/shares/service.go
      to: pkg/blockstore/engine/engine.go
      via: "engine constructor accepts BlockLayout (or BlockStoreConfig with field)"
      pattern: "BlockLayout"
    - from: pkg/blockstore/engine/fetch.go
      to: pkg/blockstore/engine/engine.go
      via: "Syncer.blockLayout reference inside dispatchRemoteFetch / inlineFetchOrWait"
      pattern: "blockLayout == .*CASOnly"
---

<objective>
Wire the per-share `BlockLayout` flag into the engine's read path. When a share is `cas-only`, the dual-read shim must NOT silently fall through to the legacy `{payloadID}/block-{idx}` path on a CAS miss — that fallback exists for unmigrated shares only. (MIG-03, D-A6, D-A8.)

Purpose: Without this gate, a single binary running mixed shares (some migrated, some not) would have the same fallback behavior on both, defeating the point of per-share migration. Also: a programming error or stale FileBlock row pointing at a legacy key on a fully-migrated share is a live-data-loss signal that must surface, not be masked by a successful legacy read.

Output: Engine routing reads BlockLayout at share open, gates the legacy fallback behind it, returns ErrLegacyReadOnCASOnly on violation. Existing dual-read tests stay green.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-01-SUMMARY.md
@.planning/phases/11-cas-write-path-gc-rewrite-a2/11-CONTEXT.md
@.planning/codebase/ARCHITECTURE.md

<interfaces>
<!-- The engine seam where the gate lands. -->

From pkg/blockstore/engine/engine.go:
```go
type BlockStore struct {
    // ... existing fields ...
    syncer *Syncer
    // NEW (this plan): blockLayout metadata.BlockLayout
}
```

From pkg/blockstore/engine/fetch.go (existing, post-Phase-11):
```go
func (m *Syncer) dispatchRemoteFetch(ctx context.Context, fb *blockstore.FileBlock) (string, []byte, error) {
    if !fb.Hash.IsZero() {
        // CAS path (verified read)
    }
    // Legacy path: ReadBlock by FormatStoreKey(payloadID, blockIdx)
}
```

From pkg/controlplane/runtime/shares/service.go (existing):
```go
func (s *Service) createBlockStoreForShare(
    ctx context.Context,
    share *Share,
    config *ShareConfig,
    blockStoreProvider BlockStoreConfigProvider,
    metadataStore metadata.MetadataStore,
    localStoreDefaults *LocalStoreDefaults,
    syncerDefaults *SyncerDefaults,
) error {
    // builds engine.BlockStore — must read share.Options.BlockLayout and
    // forward it to the engine constructor (or a setter).
}
```

From pkg/metadata/types.go (Plan 01 output):
```go
type BlockLayout string
const (
    BlockLayoutLegacy  BlockLayout = "legacy"
    BlockLayoutCASOnly BlockLayout = "cas-only"
)
func ParseBlockLayout(s string) (BlockLayout, error)
```
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Add ErrLegacyReadOnCASOnly + per-share gate in dispatchRemoteFetch</name>
  <files>pkg/blockstore/engine/engine.go, pkg/blockstore/engine/fetch.go, pkg/blockstore/engine/errors.go</files>
  <read_first>
    - pkg/blockstore/engine/engine.go (lines 1–250 — BlockStore struct definition + constructor; the new field is added here)
    - pkg/blockstore/engine/fetch.go (full file — dispatchRemoteFetch is the routing decision point; the gate lives there)
    - pkg/blockstore/engine/errors.go (or wherever engine sentinels live — grep `var Err` in pkg/blockstore/engine/ to locate)
    - pkg/blockstore/types.go (already-present BlockLayout? grep — Plan 01 added it under pkg/metadata, not pkg/blockstore, so the engine imports `metadata` for this type; confirm no import cycle by checking what pkg/blockstore/engine already imports from pkg/metadata)
  </read_first>
  <behavior>
    - Test 1 (RED first): TestDualRead_CASOnly_RefusesLegacyFallback — given a BlockStore configured with BlockLayoutCASOnly and a FileBlock with `Hash.IsZero() == true` (legacy row), calling the inline fetch path returns `ErrLegacyReadOnCASOnly` wrapped, and an Error-level log line is emitted.
    - Test 2: TestDualRead_Legacy_AllowsBothPaths — given BlockLayoutLegacy and the same legacy FileBlock, the existing legacy ReadBlock path is invoked (test stub on remoteStore returns bytes; assertion: bytes flow through).
    - Test 3: TestDualRead_CASOnly_AllowsCASPath — given BlockLayoutCASOnly and a FileBlock with non-zero Hash, the CAS verified-read path is invoked (no gate hit).
  </behavior>
  <action>
    1. Add the sentinel in `pkg/blockstore/engine/errors.go` (or the existing engine sentinel file):
       ```go
       // ErrLegacyReadOnCASOnly is returned when the engine attempts a legacy
       // {payloadID}/block-{idx} fallback on a share whose BlockLayout has
       // been flipped to cas-only. Surfacing this is a fail-loud signal —
       // either the share's metadata still contains stale legacy FileBlock
       // rows (migration bug) or a write is racing the cutover (Plan 05
       // ensures the cutover is offline-only). Operator action: re-run
       // `dfsctl blockstore migrate --share <name>` or roll the BlockLayout
       // back to legacy via direct DB intervention.
       var ErrLegacyReadOnCASOnly = errors.New("legacy read attempted on cas-only share (MIG-03)")
       ```

    2. In `pkg/blockstore/engine/engine.go`, add a field on the `Syncer` struct (or `BlockStore` — pick the one that already owns share-scoped config; grep for where `shareName` or `payloadStoreType` lives):
       ```go
       type Syncer struct {
           // ... existing fields ...
           blockLayout metadata.BlockLayout // legacy | cas-only; Plan 14-01
       }
       ```

       And in the constructor (look for `NewSyncer` or `NewBlockStore`), accept and store the value. Default to `BlockLayoutLegacy` if a zero-value is passed (defensive — matches the empty-string coercion in ParseBlockLayout from Plan 01).

    3. In `pkg/blockstore/engine/fetch.go`, modify `dispatchRemoteFetch`:
       ```go
       func (m *Syncer) dispatchRemoteFetch(ctx context.Context, fb *blockstore.FileBlock) (string, []byte, error) {
           if fb == nil {
               return "", nil, nil
           }
           if !fb.Hash.IsZero() {
               // CAS path — unchanged
               key := fb.BlockStoreKey
               if key == "" {
                   key = blockstore.FormatCASKey(fb.Hash)
               }
               data, err := m.remoteStore.ReadBlockVerified(ctx, key, fb.Hash)
               return key, data, err
           }
           // Legacy path — gated by BlockLayout per MIG-03 / D-A8.
           if m.blockLayout == metadata.BlockLayoutCASOnly {
               logger.Error("legacy FileBlock encountered on cas-only share — possible migration drift",
                   "block_id", fb.ID, "payload_id", fb.PayloadID, "block_idx", fb.BlockIndex)
               return "", nil, fmt.Errorf("%w: block_id=%s", ErrLegacyReadOnCASOnly, fb.ID)
           }
           // Existing legacy ReadBlock path follows
           // ... (unchanged) ...
       }
       ```

    4. Add `engine_dualread_test.go` cases. The existing file already contains dual-read tests — extend it with the three behaviors above. Use the existing test fixture pattern (grep `func TestDualRead` in the file to confirm shape; mirror the setup, add three new top-level Test functions, each constructs a Syncer with a stub `remoteStore` + the desired `blockLayout` and calls `dispatchRemoteFetch` directly).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./pkg/blockstore/engine/ -run 'TestDualRead_CASOnly_RefusesLegacyFallback|TestDualRead_Legacy_AllowsBothPaths|TestDualRead_CASOnly_AllowsCASPath' -count=1 &amp;&amp; grep -q 'ErrLegacyReadOnCASOnly' pkg/blockstore/engine/fetch.go &amp;&amp; grep -q 'BlockLayoutCASOnly' pkg/blockstore/engine/fetch.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'ErrLegacyReadOnCASOnly' pkg/blockstore/engine/` recursively (excluding test files) returns at least 2 (sentinel + reference in fetch.go).
    - `grep -c 'blockLayout' pkg/blockstore/engine/engine.go pkg/blockstore/engine/syncer.go pkg/blockstore/engine/fetch.go` (whichever holds Syncer) returns at least 3 across the three locations (struct field + constructor + dispatchRemoteFetch gate).
    - `go test ./pkg/blockstore/engine/ -run 'TestDualRead' -count=1` passes (existing dual-read tests AND the new three tests).
    - `go vet ./pkg/blockstore/engine/` clean.
  </acceptance_criteria>
  <done>
    Engine refuses legacy fallback on cas-only shares with ErrLegacyReadOnCASOnly; legacy and cas-only paths green-tested.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Pass BlockLayout from share service into engine constructor</name>
  <files>pkg/controlplane/runtime/shares/service.go, pkg/blockstore/engine/engine.go</files>
  <read_first>
    - pkg/controlplane/runtime/shares/service.go (search for `createBlockStoreForShare` and the engine.NewBlockStore / NewSyncer call site — that is the wiring point)
    - pkg/blockstore/engine/engine.go (the constructor signature — confirm whether config passes through a struct or positional args; pick the one that already exists and add the new field there)
  </read_first>
  <behavior>
    - Test 1: After `AddShare(config{Options:{BlockLayout:CASOnly}})`, the resulting `*engine.BlockStore` reports `BlockLayout() == CASOnly` (add a getter for testability).
    - Test 2: After `AddShare(config{Options:{BlockLayout:""}})`, the resulting BlockStore reports `BlockLayout() == Legacy` (zero-value coercion).
    - Test 3: After `AddShare(config{Options:{BlockLayout:Legacy}})`, the resulting BlockStore reports `BlockLayout() == Legacy`.
  </behavior>
  <action>
    1. In `pkg/blockstore/engine/engine.go`:
       - If the engine constructor takes a `Config` struct, add `BlockLayout metadata.BlockLayout` to it.
       - If it takes positional args, prefer adding a `Config` field; if that's too invasive, take a setter `func (bs *BlockStore) SetBlockLayout(bl metadata.BlockLayout)` called by the share service before the engine starts serving reads. Choose the smaller diff path.
       - Add a getter `func (bs *BlockStore) BlockLayout() metadata.BlockLayout` for testability and for Plan 05's auto-cutover (which needs to flip it at runtime).
       - On construction, coerce empty BlockLayout to BlockLayoutLegacy (defense in depth — backends already do this on read, but the engine should not assume).

    2. In `pkg/controlplane/runtime/shares/service.go`:
       - Inside `createBlockStoreForShare`, fetch the share's `BlockLayout` from `metadataStore.GetShareOptions(ctx, shareName)`. The metadata store is already accessible inside this function (see method signature). Do NOT re-fetch from the controlplane DB — the metadata store is the source of truth per D-A6.
       - Pass the BlockLayout into the engine constructor / setter.

    3. Add a regression test in `pkg/controlplane/runtime/shares/service_test.go` (or wherever existing AddShare tests live; grep `func TestService_AddShare` first) covering Tests 1–3 above. The test should use a memory metadata store + a memory block store — no real S3 needed.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go vet ./pkg/controlplane/runtime/shares/ ./pkg/blockstore/engine/ &amp;&amp; grep -q 'BlockLayout' pkg/controlplane/runtime/shares/service.go &amp;&amp; grep -q 'func.*BlockStore.*BlockLayout' pkg/blockstore/engine/engine.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'BlockLayout' pkg/controlplane/runtime/shares/service.go` >= 2 (read from ShareOptions + pass into engine).
    - `grep -c 'func.*BlockStore.*BlockLayout' pkg/blockstore/engine/engine.go` >= 1 (getter).
    - `go build ./...` succeeds across the entire module.
    - `go vet ./pkg/controlplane/runtime/shares/ ./pkg/blockstore/engine/` clean.
    - The 3 wiring tests pass.
  </acceptance_criteria>
  <done>
    Engine BlockStore knows its share's BlockLayout from the metadata store at AddShare time; getter present for cutover and tests.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Metadata store row → engine | The block_layout value crosses from operator-controlled persistence into the read-path decision. |
| Stale FileBlock row on cas-only share → caller bytes | A legacy-keyed FileBlock row that survives migration would silently serve old bytes if the gate were absent — the gate's whole purpose. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-02-01 | Tampering | Direct DB UPDATE flipping BlockLayout to cas-only on an unmigrated share | mitigate | Engine reads BlockLayout once at share-open. Plan 05 controls the production-flip path. Direct DB tampering surfaces as `ErrLegacyReadOnCASOnly` on first legacy-keyed read — fail-loud per D-A8. |
| T-14-02-02 | Information disclosure | Silent legacy fallback returning stale post-migration bytes | mitigate | Per-share gate refuses the fallback on cas-only. The error is logged at Error level with block_id and payload_id for forensic triage. |
| T-14-02-03 | Denial of service | Existing legacy share unable to read after this change | mitigate | Default coercion of empty/zero BlockLayout to `legacy` ensures pre-Phase-14 shares behave identically to today. The dual-read tests assert this. |
</threat_model>

<verification>
- Existing dual-read tests pass unchanged.
- Three new tests assert: cas-only refuses legacy fallback / legacy allows both / cas-only allows CAS path.
- Engine constructor / share service wiring tests pass.
- `go build ./...` and `go vet ./...` green.
</verification>

<success_criteria>
- ErrLegacyReadOnCASOnly sentinel landed.
- Engine reads BlockLayout from share at open time.
- Per-share gate inside dispatchRemoteFetch refuses legacy fallback on cas-only shares.
- Three new dual-read tests + three wiring tests pass.
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-02-SUMMARY.md` describing the new sentinel, the engine field/getter, and the share-service wiring touch.
</output>
