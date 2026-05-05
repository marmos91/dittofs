---
phase: 14-migration-tool-a5
plan: 01
type: execute
wave: 1
depends_on: []
files_modified:
  - pkg/metadata/types.go
  - pkg/metadata/store/memory/shares.go
  - pkg/metadata/store/badger/shares.go
  - pkg/metadata/store/postgres/shares.go
  - pkg/metadata/storetest/shares_blocklayout_test.go
autonomous: true
requirements: [MIG-03]
tags: [block_layout, share, metadata, conformance]
must_haves:
  truths:
    - "Every metadata.ShareOptions value carries a BlockLayout field with values 'legacy' or 'cas-only'"
    - "Memory, Badger, and Postgres backends round-trip the BlockLayout field through CreateShare/GetShareOptions/UpdateShareOptions"
    - "New shares default to BlockLayout='cas-only'; the helper that reads the field returns 'legacy' if a backend produced an empty string (forward-compat for old DB rows)"
    - "storetest conformance suite asserts the BlockLayout round-trip across all three backends"
  artifacts:
    - path: pkg/metadata/types.go
      provides: "BlockLayout enum constants + field on ShareOptions + ParseBlockLayout helper"
      contains: "BlockLayoutLegacy"
    - path: pkg/metadata/store/memory/shares.go
      provides: "BlockLayout copied through CreateShare and UpdateShareOptions on the in-memory shareData"
      contains: "BlockLayout"
    - path: pkg/metadata/store/postgres/shares.go
      provides: "BlockLayout column read/write on the shares row"
      contains: "block_layout"
    - path: pkg/metadata/store/badger/shares.go
      provides: "BlockLayout encoded into the Badger share JSON record"
      contains: "BlockLayout"
    - path: pkg/metadata/storetest/shares_blocklayout_test.go
      provides: "RunBlockLayoutSuite conformance scenarios driven by all three backend test entry points"
      contains: "RunBlockLayoutSuite"
  key_links:
    - from: pkg/metadata/types.go
      to: pkg/metadata/store/{memory,badger,postgres}/shares.go
      via: "ShareOptions struct field"
      pattern: "options\\.BlockLayout"
    - from: pkg/metadata/storetest/shares_blocklayout_test.go
      to: pkg/metadata/store/{memory,badger,postgres}
      via: "exported test entry function (existing convention)"
      pattern: "RunBlockLayoutSuite"
---

<objective>
Add the per-share `block_layout` field to the metadata-store share record, persist it across all three backends (Memory, Badger, Postgres), and add a conformance scenario that asserts the field round-trips. This is the foundation Plan 02 (engine routing) and Plans 03–05 (migration tool) build on. (D-A6, MIG-03.)

Purpose: Enables per-share gating of the dual-read shim. Without this field, a single binary cannot mix `legacy` shares (mid-migration) and `cas-only` shares (already migrated) at the same time.

Output: ShareOptions.BlockLayout, three backend impls, storetest conformance scenario that all three backends pass.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/ROADMAP.md
@.planning/STATE.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/codebase/CONVENTIONS.md

<interfaces>
<!-- Existing share metadata seam — extracted to spare the executor a codebase scavenger hunt. -->

From pkg/metadata/types.go:
```go
type Share struct {
    Name    string
    Options ShareOptions
}

type ShareOptions struct {
    ReadOnly             bool
    Async                bool
    AllowedClients       []string
    DeniedClients        []string
    RequireAuth          bool
    AllowedAuthMethods   []string
    IdentityMapping      *IdentityMapping
    // NEW (this plan): BlockLayout BlockLayout
}
```

From pkg/metadata/store.go (excerpt):
```go
GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error)
UpdateShareOptions(ctx context.Context, shareName string, options *ShareOptions) error
CreateShare(ctx context.Context, share *Share) error
```

From pkg/metadata/store/memory/shares.go (existing pattern, illustrative):
```go
// shareData is the in-memory record. CreateShare copies *share into it;
// UpdateShareOptions writes options into shareData.Share.Options. The new
// BlockLayout field follows the same path with no special handling.
shareData.Share.Options = *options
```

Postgres conventions (existing migrations live under
pkg/metadata/store/postgres/migrations/, numbered 0000XX-name.up.sql /
.down.sql; see Phase 12 migration 000012 + Phase 13 migration 000013 as
references for column-add migrations and how the backend Read/Write code
gates on the new column).

Badger conventions: shares are stored as a single JSON blob keyed by
share name; backend reads/writes go through json.Marshal/Unmarshal of a
struct that mirrors the in-memory shape — adding a tagged field is
forward-compat by default (older blobs simply omit it; BlockLayout
helper coerces "" to legacy).
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Add BlockLayout enum + field to metadata.ShareOptions</name>
  <files>pkg/metadata/types.go, pkg/metadata/types_test.go</files>
  <read_first>
    - pkg/metadata/types.go (lines 100–155 — Share / ShareOptions definitions; the new field goes inside ShareOptions)
    - pkg/blockstore/retention.go (existing convention for parsing string-enum fields — ParseRetentionPolicy is the pattern to mirror; see also pkg/controlplane/models/share.go GetRetentionPolicy for how callers consume it with empty-string fallback)
  </read_first>
  <behavior>
    - Test 1: `ParseBlockLayout("legacy")` returns `BlockLayoutLegacy`, nil error.
    - Test 2: `ParseBlockLayout("cas-only")` returns `BlockLayoutCASOnly`, nil error.
    - Test 3: `ParseBlockLayout("")` returns `BlockLayoutLegacy`, nil error (empty-string forward-compat for old DB rows that pre-date the field — matches the legacy default semantic in D-A6).
    - Test 4: `ParseBlockLayout("invalid")` returns the zero value and a non-nil error wrapping `ErrInvalidBlockLayout`.
    - Test 5: `String()` round-trip — `BlockLayoutCASOnly.String() == "cas-only"`, `BlockLayoutLegacy.String() == "legacy"`.
    - Test 6: zero-value `ShareOptions{}` reports `Options.BlockLayout == BlockLayoutLegacy` after a `Normalize()`/coerce step (or the Get* helper, mirror the retention pattern).
  </behavior>
  <action>
    Edit `pkg/metadata/types.go`:

    1. Add the enum near the existing `KerberosLevel` constants block:
       ```go
       // BlockLayout names the block-key scheme a share is currently using.
       // Per-share gate for the dual-read shim during the v0.13/v0.14 -> v0.15
       // migration window (MIG-03, D-A6). Greenfield v0.15 shares default to
       // cas-only; shares created on v0.13 or v0.14 default to legacy and are
       // flipped to cas-only by `dfsctl blockstore migrate`.
       type BlockLayout string

       const (
           BlockLayoutLegacy  BlockLayout = "legacy"
           BlockLayoutCASOnly BlockLayout = "cas-only"
       )

       // ErrInvalidBlockLayout is returned by ParseBlockLayout for unknown values.
       var ErrInvalidBlockLayout = errors.New("invalid block_layout")

       // ParseBlockLayout parses a string into a BlockLayout. The empty string
       // returns BlockLayoutLegacy so that pre-Phase-14 DB rows (which lack the
       // column or have null/empty values) read as `legacy` — the safe default
       // because the dual-read shim must remain active until proven otherwise.
       func ParseBlockLayout(s string) (BlockLayout, error) {
           switch s {
           case "":
               return BlockLayoutLegacy, nil
           case string(BlockLayoutLegacy):
               return BlockLayoutLegacy, nil
           case string(BlockLayoutCASOnly):
               return BlockLayoutCASOnly, nil
           default:
               return "", fmt.Errorf("%w: %q", ErrInvalidBlockLayout, s)
           }
       }

       func (b BlockLayout) String() string { return string(b) }
       ```

    2. Add the field to `ShareOptions`:
       ```go
       type ShareOptions struct {
           // ... existing fields ...

           // BlockLayout selects the block-key scheme used by this share's
           // engine. New shares created post-Phase-14 default to cas-only.
           // v0.13/v0.14 imports default to legacy; the dual-read shim
           // serves both legacy {payloadID}/block-{idx} and cas/.../h reads
           // until `dfsctl blockstore migrate` flips this to cas-only.
           BlockLayout BlockLayout
       }
       ```

    3. Add the `errors` import if missing. Confirm `fmt` already imported.

    4. In `pkg/metadata/types_test.go` add `TestParseBlockLayout` and `TestBlockLayout_String` covering Tests 1–5 above. Use `github.com/stretchr/testify/require` (matches existing convention in this package — grep for `require.NoError` to confirm before importing).

    Do NOT yet wire any backend reads/writes — that is Task 2 + Task 3. This task lands the type so the storetest scaffolding compiles.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./pkg/metadata/ -run 'TestParseBlockLayout|TestBlockLayout_String' -count=1 &amp;&amp; go vet ./pkg/metadata/ &amp;&amp; grep -q 'BlockLayoutCASOnly' pkg/metadata/types.go &amp;&amp; grep -q 'BlockLayout BlockLayout' pkg/metadata/types.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c '^const' pkg/metadata/types.go` is unchanged (the new constants live in an existing const block) OR a new `BlockLayout` const block exists — either is fine.
    - `grep -c 'BlockLayout' pkg/metadata/types.go` >= 6 (type + 2 constants + 1 sentinel + parse fn + ShareOptions field).
    - `go test ./pkg/metadata/ -run 'TestParseBlockLayout|TestBlockLayout_String' -count=1` passes.
    - `go vet ./pkg/metadata/` clean.
    - The `ShareOptions` literal in any existing struct initialization site that used to compile still compiles (the new field is zero-value safe — empty BlockLayout will be coerced via the Get* helper introduced in Task 2; do not require callers to update).
  </acceptance_criteria>
  <done>
    BlockLayout type, two constants (legacy / cas-only), ParseBlockLayout helper with empty-string forward-compat, ShareOptions.BlockLayout field landed; unit tests in types_test.go pass.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Persist BlockLayout in Memory + Badger + Postgres backends</name>
  <files>
    pkg/metadata/store/memory/shares.go,
    pkg/metadata/store/badger/shares.go,
    pkg/metadata/store/postgres/shares.go,
    pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql,
    pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.down.sql
  </files>
  <read_first>
    - pkg/metadata/store/memory/shares.go (full file — small; see how shareData mirrors metadata.Share + RootHandle, and how UpdateShareOptions mutates it)
    - pkg/metadata/store/badger/shares.go (look for the JSON encoding struct used to serialize the share record; the new field goes there with a json tag)
    - pkg/metadata/store/postgres/shares.go (find the SELECT / INSERT / UPDATE statements covering ShareOptions columns; this is where block_layout is read and written)
    - pkg/metadata/store/postgres/migrations/000013_*.up.sql (most recent migration, copy its style — `ALTER TABLE shares ADD COLUMN ...`)
    - pkg/metadata/store/postgres/migrations/000013_*.down.sql (mirror style)
  </read_first>
  <behavior>
    - Test 1: After `CreateShare(share{Options:{BlockLayout: BlockLayoutCASOnly}})`, a follow-up `GetShareOptions(name)` returns `BlockLayout == BlockLayoutCASOnly`.
    - Test 2: After `CreateShare` with empty BlockLayout (zero value), `GetShareOptions` returns `BlockLayoutLegacy` (the empty-string coercion lives in ParseBlockLayout from Task 1; backends should call ParseBlockLayout on read).
    - Test 3: `UpdateShareOptions` from `legacy` -> `cas-only` is observable via the next `GetShareOptions` call.
    - Test 4 (Postgres only): the migration 000014 up + down are reversible — apply up, observe column exists; apply down, observe column does not exist.
  </behavior>
  <action>
    **Memory** (`pkg/metadata/store/memory/shares.go`):
    - `CreateShare`: copy `share.Options.BlockLayout` into `shareData.Share.Options.BlockLayout` — no work needed beyond what already happens because `shareData{Share: *share}` is a value copy. Confirm via grep.
    - `GetShareOptions`: after the existing `optsCopy := shareData.Share.Options` line, normalize: `if normalized, err := metadata.ParseBlockLayout(string(optsCopy.BlockLayout)); err == nil { optsCopy.BlockLayout = normalized }` (silently coerce empty / unknown to legacy — matches D-A6 default).
    - `UpdateShareOptions`: existing `shareData.Share.Options = *options` already covers it. No code change.

    **Badger** (`pkg/metadata/store/badger/shares.go`):
    - Locate the JSON encoding struct used to serialize the share record into Badger (likely named `shareRecord` or `persistedShare` — grep within the file for `json.Marshal`).
    - Add the field: `BlockLayout metadata.BlockLayout `json:"block_layout,omitempty"``.
    - On read, after json.Unmarshal, run `BlockLayout` through ParseBlockLayout to coerce empty → legacy.
    - On `CreateShare` and `UpdateShareOptions`, write the field through unchanged (Marshal handles it).

    **Postgres** (`pkg/metadata/store/postgres/shares.go`):
    - Add migration files at exact paths:
      - `pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql` containing:
        ```sql
        ALTER TABLE shares ADD COLUMN block_layout TEXT NOT NULL DEFAULT 'legacy';
        ```
      - `pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.down.sql` containing:
        ```sql
        ALTER TABLE shares DROP COLUMN block_layout;
        ```
      Why `DEFAULT 'legacy'`: existing rows pre-Phase-14 must be readable as legacy until migrated (D-A6). New greenfield shares created via API will pass `cas-only` explicitly via `CreateShare`.
    - Update the SELECT statement in `GetShareOptions` to include `block_layout`; scan into a string variable then `ParseBlockLayout`.
    - Update INSERT in `CreateShare` to include `block_layout` column (use `string(share.Options.BlockLayout)`; if empty, write `"legacy"`).
    - Update UPDATE in `UpdateShareOptions` to include `block_layout = $N`.
    - If Postgres uses a `migrate` library that requires no explicit registration (file-system-based migrations), skip registration step. If it requires editing `pkg/metadata/store/postgres/migrations.go` (or similar) to embed the new file via `embed.FS`, do that too — grep for `embed.FS` in `pkg/metadata/store/postgres/` to determine.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./pkg/metadata/store/... &amp;&amp; go vet ./pkg/metadata/store/... &amp;&amp; ls pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.down.sql &amp;&amp; grep -c 'block_layout' pkg/metadata/store/postgres/shares.go | awk '$1 &gt;= 3 {print "ok"; exit 0} {exit 1}'</automated>
  </verify>
  <acceptance_criteria>
    - Files `pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql` and `.down.sql` exist.
    - `grep -c 'block_layout' pkg/metadata/store/postgres/shares.go` is at least 3 (SELECT, INSERT, UPDATE).
    - `grep -c 'BlockLayout' pkg/metadata/store/badger/shares.go` is at least 2 (JSON struct field + read coercion).
    - `grep -c 'ParseBlockLayout' pkg/metadata/store/memory/shares.go` is at least 1.
    - `go build ./pkg/metadata/store/...` succeeds; `go vet ./pkg/metadata/store/...` clean.
    - The Postgres migration runs forward and backward against an empty schema (gated by Task 3 conformance test, no separate command needed here).
  </acceptance_criteria>
  <done>
    All three metadata-store backends round-trip `ShareOptions.BlockLayout`. Postgres migration 000014 is reversible. Empty / missing values coerce to `legacy` per D-A6.
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 3: storetest conformance — RunBlockLayoutSuite scenarios</name>
  <files>
    pkg/metadata/storetest/shares_blocklayout_test.go,
    pkg/metadata/store/memory/shares_test.go,
    pkg/metadata/store/badger/shares_test.go,
    pkg/metadata/store/postgres/shares_test.go
  </files>
  <read_first>
    - pkg/metadata/storetest/suite.go (or whichever file holds the existing `Run*Suite` exported test entrypoints — grep `func Run` in pkg/metadata/storetest/ to find the canonical pattern; copy it)
    - pkg/metadata/store/memory/shares_test.go (or similar — the existing per-backend hookup that calls into storetest)
    - pkg/metadata/store/badger/shares_test.go (existing hookup)
    - pkg/metadata/store/postgres/shares_test.go (existing hookup; Postgres tests typically guard on a TEST_POSTGRES_URL env var — preserve that gate)
  </read_first>
  <behavior>
    - Scenario 1 — RoundTripCASOnly: CreateShare with BlockLayout=cas-only -> GetShareOptions returns cas-only.
    - Scenario 2 — RoundTripLegacy: CreateShare with BlockLayout=legacy -> GetShareOptions returns legacy.
    - Scenario 3 — DefaultLegacyOnEmpty: CreateShare with BlockLayout="" (zero value) -> GetShareOptions returns BlockLayoutLegacy.
    - Scenario 4 — UpdateLegacyToCASOnly: CreateShare(legacy) -> UpdateShareOptions(cas-only) -> GetShareOptions returns cas-only.
    - Scenario 5 — UpdateCASOnlyToLegacy: round-trips both directions cleanly (defensive — migration is forward-only, but the field is symmetric).
  </behavior>
  <action>
    1. Create `pkg/metadata/storetest/shares_blocklayout_test.go` exporting:
       ```go
       // RunBlockLayoutSuite asserts that a metadata.MetadataStore round-trips
       // ShareOptions.BlockLayout across CreateShare / GetShareOptions /
       // UpdateShareOptions. Backends invoke this from their per-backend test
       // file. Conformance gate for MIG-03 (D-A6).
       func RunBlockLayoutSuite(t *testing.T, factory StoreFactory) { ... }
       ```
       Implement the 5 scenarios above as `t.Run(...)` subtests. Each scenario builds a fresh share via `factory(t)`, creates the share, exercises Get/Update/Get, and asserts using `require`.

       The signature `StoreFactory` is whatever the storetest package already uses — grep `type StoreFactory` or look at how existing `Run*Suite` functions parameterize the backend. Mirror that exactly. Do NOT introduce a new factory shape.

    2. Wire it into the three backends. In each `pkg/metadata/store/{memory,badger,postgres}/shares_test.go`, add a function:
       ```go
       func TestBlockLayoutConformance(t *testing.T) {
           storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.MetadataStore {
               // existing per-backend setup helper goes here -- copy from
               // sibling Run*Suite invocation at the top of the file
           })
       }
       ```
       Do not invent a new fixture pattern; the backends already have a working factory closure for prior conformance suites — reuse it verbatim.

    3. Postgres: ensure the conformance test is guarded by the same env-var gate the existing tests use (typically `if os.Getenv("TEST_POSTGRES_URL") == "" { t.Skip(...) }` or a helper function). If Postgres tests are normally run via a dedicated CI lane, do not break that gate.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go test ./pkg/metadata/store/memory/ -run TestBlockLayoutConformance -count=1 &amp;&amp; go test ./pkg/metadata/store/badger/ -run TestBlockLayoutConformance -count=1 &amp;&amp; go vet ./pkg/metadata/storetest/...</automated>
  </verify>
  <acceptance_criteria>
    - File `pkg/metadata/storetest/shares_blocklayout_test.go` exists with exported `RunBlockLayoutSuite(t *testing.T, factory ...)`.
    - `grep -c 'RunBlockLayoutSuite' pkg/metadata/store/memory/shares_test.go` >= 1.
    - `grep -c 'RunBlockLayoutSuite' pkg/metadata/store/badger/shares_test.go` >= 1.
    - `grep -c 'RunBlockLayoutSuite' pkg/metadata/store/postgres/shares_test.go` >= 1.
    - Memory + Badger backend tests pass via the bundled `go test` invocation. Postgres test compiles (run only when `TEST_POSTGRES_URL` is set; skip otherwise — match existing convention).
    - The 5 named subtests appear when running with `-v`: `RoundTripCASOnly`, `RoundTripLegacy`, `DefaultLegacyOnEmpty`, `UpdateLegacyToCASOnly`, `UpdateCASOnlyToLegacy`.
  </acceptance_criteria>
  <done>
    Conformance suite exists and is invoked by all three backends; Memory and Badger pass green in CI; Postgres pass green under the existing env-gated lane.
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| metadata-store driver → backing store | The new `block_layout` column is operator-controlled (set via API/Cobra share-create flow). Trusted input — no end-user write path. |
| Old DB row → new code | Pre-Phase-14 DB rows lack the column. Coercion via `ParseBlockLayout("")` returns `legacy`, which keeps the dual-read shim active — fail-safe. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-01-01 | Tampering | Operator-supplied value bypassing enum | mitigate | `ParseBlockLayout` rejects unknown strings; backends call it on every read so a row with a bogus value (e.g., manual `psql UPDATE`) returns `ErrInvalidBlockLayout` rather than silently being treated as cas-only. |
| T-14-01-02 | Information disclosure | Migration not yet performed but flag flipped | mitigate | Coercion of empty string to `legacy` is the safe default — never coerce unknown to cas-only. The flip to cas-only happens only via the migration tool (Plan 05), gated on the integrity check passing. |
| T-14-01-03 | Denial of service | Down migration (000014) on a populated table dropping the column | accept | Down migration is operator-initiated, only used during dev/test rollback. Production rollback uses Phase-15 deletion path. Standard ALTER TABLE DROP COLUMN cost on Postgres is acceptable. |
</threat_model>

<verification>
- All three backends round-trip `ShareOptions.BlockLayout`.
- Postgres migration 000014 is up-down reversible.
- Empty / missing field coerces to `legacy`.
- Unknown values surface as `ErrInvalidBlockLayout` on backend read.
- `go build ./...` and `go vet ./pkg/metadata/...` clean.
</verification>

<success_criteria>
- BlockLayout type + ShareOptions.BlockLayout field landed.
- Memory + Badger + Postgres backends persist + read the field.
- storetest `RunBlockLayoutSuite` is invoked from all three backend test files; Memory and Badger pass green.
- Postgres migration 000014 file pair exists and is reversible against an empty schema.
</success_criteria>

<output>
After completion, create `.planning/phases/14-migration-tool-a5/14-01-SUMMARY.md` summarizing the new field, the three backend touchpoints, and the conformance suite hookup.
</output>
