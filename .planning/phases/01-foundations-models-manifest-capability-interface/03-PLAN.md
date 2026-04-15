---
phase: 01-foundations-models-manifest-capability-interface
plan: 03
type: execute
wave: 1
depends_on: []
files_modified:
  - pkg/backup/manifest/manifest.go
  - pkg/backup/manifest/manifest_test.go
  - pkg/metadata/backup.go
  - pkg/metadata/backup_test.go
autonomous: true
requirements: [SAFETY-03, ENG-04]

must_haves:
  truths:
    - "Manifest v1 YAML struct exists with all 9 required top-level fields (manifest_version, backup_id, created_at, store_id, store_kind, sha256, size_bytes, encryption, payload_id_set) plus optional engine_metadata"
    - "Manifest.Validate() rejects manifest_version == 0 with 'manifest_version is required'"
    - "Manifest.Validate() rejects manifest_version != CurrentVersion with 'unsupported manifest_version' (forward-compat guard, SAFETY-03)"
    - "Marshal → Parse round-trip preserves all fields byte-for-byte on a fully-populated manifest"
    - "payload_id_set field is a YAML list of strings — present in serialized output even when empty (empty slice, NOT omitempty) because it's critical for SAFETY-01 GC hold in Phase 5"
    - "Marshaling the same manifest twice produces byte-identical output (deterministic — needed so SHA-256 checksum is reproducible)"
    - "Backupable interface declared in pkg/metadata/backup.go with exact signature Backup(ctx, io.Writer) (PayloadIDSet, error); Restore(ctx, io.Reader) error"
    - "PayloadIDSet type exists with Add, Contains, Len methods — backed by map[string]struct{}"
    - "ErrBackupUnsupported sentinel exists and is returnable from capability checks (ENG-04)"
    - "No metadata store implementation is modified in this plan — Backupable is interface-only; Phase 2 wires impls"
  artifacts:
    - path: "pkg/backup/manifest/manifest.go"
      provides: "Manifest struct, Encryption struct, CurrentVersion const, Marshal/WriteTo/Parse/ReadFrom/Validate"
      contains: "const CurrentVersion = 1"
    - path: "pkg/backup/manifest/manifest_test.go"
      provides: "Round-trip, version-guard, required-fields, determinism, WriteTo/ReadFrom tests"
      exports: []
    - path: "pkg/metadata/backup.go"
      provides: "Backupable interface, PayloadIDSet type + methods, ErrBackupUnsupported sentinel"
      contains: "type Backupable interface"
    - path: "pkg/metadata/backup_test.go"
      provides: "PayloadIDSet unit tests, ErrBackupUnsupported identity test"
      exports: []
  key_links:
    - from: "pkg/backup/manifest/manifest.go"
      to: "gopkg.in/yaml.v3"
      via: "yaml.Marshal / yaml.Unmarshal"
      pattern: "yaml\\.(Marshal|Unmarshal)"
    - from: "pkg/metadata/backup.go (Backupable)"
      to: "Phase 2 per-store drivers (memory, badger, postgres)"
      via: "Interface contract, implemented by concrete stores in Phase 2"
      pattern: "Backup\\(ctx.*io\\.Writer\\).*PayloadIDSet"
    - from: "pkg/backup/manifest/manifest.go (Manifest.PayloadIDSet)"
      to: "Phase 5 block-GC hold (SAFETY-01)"
      via: "Manifest consumer in Phase 5 reads PayloadIDSet to suppress GC deletion"
      pattern: "PayloadIDSet.*\\[\\]string"
---

<objective>
Define the two stable contracts every downstream phase compiles against:

1. **Manifest v1** — versioned, self-describing YAML descriptor that each backup writes (SAFETY-03).
2. **`Backupable` capability interface** — stream-based contract that Phase 2 metadata store drivers implement (ENG-04).

Purpose: These are pure contracts. No drivers, no handlers. Once this plan lands, Phase 2 teams can build engine drivers against the stable interface, and Phase 3 destination drivers know the manifest shape to write. The `payload_id_set` field is deliberately included now so Phase 5's block-GC hold (SAFETY-01) can read manifests from day one.

Output: `pkg/backup/manifest/manifest.go` (new package) + `pkg/metadata/backup.go` (new file) + comprehensive tests for both.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md
@.planning/phases/01-foundations-models-manifest-capability-interface/01-PATTERNS.md
@.planning/research/PITFALLS.md

# Directly relevant analogs / references
@pkg/controlplane/models/stores.go
@pkg/metadata/store.go
@pkg/metadata/errors.go

<interfaces>
<!-- Locked shape from CONTEXT.md §Manifest Format. Copy exactly. -->

```go
// pkg/backup/manifest/manifest.go
package manifest

const CurrentVersion = 1

type Encryption struct {
    Enabled   bool   `yaml:"enabled"`
    Algorithm string `yaml:"algorithm,omitempty"` // "aes-256-gcm" when Enabled
    KeyRef    string `yaml:"key_ref,omitempty"`   // env var name or file path — never the key
}

type Manifest struct {
    ManifestVersion int               `yaml:"manifest_version"`
    BackupID        string            `yaml:"backup_id"`
    CreatedAt       time.Time         `yaml:"created_at"`
    StoreID         string            `yaml:"store_id"`
    StoreKind       string            `yaml:"store_kind"`   // "memory" | "badger" | "postgres"
    SHA256          string            `yaml:"sha256"`
    SizeBytes       int64             `yaml:"size_bytes"`
    Encryption      Encryption        `yaml:"encryption"`
    PayloadIDSet    []string          `yaml:"payload_id_set"` // NOT omitempty — required even if empty
    EngineMetadata  map[string]string `yaml:"engine_metadata,omitempty"`
}
```

```go
// pkg/metadata/backup.go — interface surface
package metadata

type Backupable interface {
    Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)
    Restore(ctx context.Context, r io.Reader) error
}

type PayloadIDSet map[string]struct{}

func (s PayloadIDSet) Add(id string)            { s[id] = struct{}{} }
func (s PayloadIDSet) Contains(id string) bool  { _, ok := s[id]; return ok }
func (s PayloadIDSet) Len() int                 { return len(s) }

var ErrBackupUnsupported = errors.New("backup not supported by this metadata store")
```

Existing pkg/metadata/errors.go pattern (for consistency; ErrBackupUnsupported lives in backup.go not here, but style matches):
```go
var ErrNotSupported = errors.New("not supported")  // existing
```

gopkg.in/yaml.v3 is already in go.mod (verified in PATTERNS.md §Dependency Verification).
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: Manifest v1 package (YAML codec + version guard)</name>
  <files>pkg/backup/manifest/manifest.go, pkg/backup/manifest/manifest_test.go</files>
  <behavior>
    - Package `manifest` under new directory `pkg/backup/manifest/`.
    - Exports listed in <interfaces> above: `CurrentVersion`, `Encryption`, `Manifest`, `Marshal`, `WriteTo`, `Parse`, `ReadFrom`, `Validate`.
    - `Marshal() ([]byte, error)` — delegates to `yaml.Marshal`.
    - `WriteTo(w io.Writer) (int64, error)` — marshal then write, return bytes written and any error.
    - `Parse([]byte) (*Manifest, error)` — `yaml.Unmarshal` then `Validate`. Wrap decode errors with `fmt.Errorf("decode manifest: %w", err)`.
    - `ReadFrom(r io.Reader) (*Manifest, error)` — `io.ReadAll` then `Parse`. Wrap read error `fmt.Errorf("read manifest: %w", err)`.
    - `Validate() error` — enforce: ManifestVersion != 0 (else "manifest_version is required"); ManifestVersion == CurrentVersion (else "unsupported manifest_version %d (this build supports %d)"); BackupID, StoreID, StoreKind, SHA256 non-empty (each returns its own field-named error).
    - PayloadIDSet field MUST be `[]string` with NO `omitempty` tag — the field appears in YAML output even when empty (empty list `[]`). This is critical: Phase 5 GC hold reads this field to know "this backup references zero blocks" (valid) vs "this field is missing" (broken backup).
    - Tests in `manifest_test.go` (plain Go test, no build tag):
      * `TestManifestRoundTrip` — populate a fully-populated Manifest (all fields non-zero, Encryption enabled, 3 PayloadIDs, engine_metadata with 2 entries); Marshal; Parse; require.Equal on all fields. Note: `time.Time` round-trip via YAML drops sub-second precision in some configs — construct the test time with `time.Now().UTC().Truncate(time.Second)` to sidestep, or use `time.Date(...)`.
      * `TestManifestVersionGuard_RejectsZero` — empty Manifest (ManifestVersion=0), Validate returns error containing "manifest_version is required".
      * `TestManifestVersionGuard_RejectsFuture` — ManifestVersion=999, all other required fields set; Validate returns error containing "unsupported manifest_version".
      * `TestManifestVersionGuard_AcceptsCurrent` — ManifestVersion=CurrentVersion, all required fields set; Validate returns nil.
      * `TestManifestRequiredFields` — table-driven with cases: missing BackupID, missing StoreID, missing StoreKind, missing SHA256. Each blank value yields Validate error containing the field name.
      * `TestManifestEncryptionOmitEmpty` — Encryption{Enabled:false} (Algorithm and KeyRef blank); marshal; assert serialized YAML does NOT contain the strings "algorithm:" or "key_ref:".
      * `TestManifestPayloadIDSetAlwaysPresent` — empty PayloadIDSet ([]string{}); marshal; assert serialized YAML CONTAINS "payload_id_set:" (even though empty). Use regex or substring match.
      * `TestManifestPayloadIDSetDeterministic` — two manifests with identical contents including sorted `[]string{"a","b","c"}` must produce byte-identical Marshal output.
      * `TestManifestWriteThenRead` — use `bytes.Buffer`; WriteTo; ReadFrom; require.Equal on recovered Manifest.
      * `TestParseRejectsBrokenYAML` — `Parse([]byte("not: valid: yaml: here:"))` returns error containing "decode manifest".
  </behavior>
  <action>
    1. Create directory `pkg/backup/manifest/`. Create `manifest.go` and `manifest_test.go`.
    2. Implement per <interfaces> block. Package doc at file top: `// Package manifest implements the v1 backup manifest format (SAFETY-03).` Inline comment above `PayloadIDSet` field: `// PayloadIDSet lists all block PayloadIDs referenced by this backup. Required for block-GC hold (SAFETY-01); must not use omitempty.`
    3. Inline comment on Validate: `// Phase 1 accepts only CurrentVersion. Future versions branch here for forward-compat (SAFETY-03).`
    4. Tests: use `github.com/stretchr/testify/require` (project already uses testify — check imports in existing `_test.go` files to confirm). Zero external fixtures; all test data is constructed in the test.
    5. PITFALL AWARENESS: from PITFALLS.md #4, manifest must be the "contract truth" for cross-store / cross-version detection. Even though Phase 5 owns the restore path, this plan must ensure `store_id` and `store_kind` validation is active in Validate() so any manifest missing these fails fast.
    6. Do NOT add `Sign`/`HMAC`/`Verify` in this phase — encryption primitives land in Phase 3. The manifest only records `Encryption{Enabled, Algorithm, KeyRef}` metadata.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go build ./pkg/backup/manifest/... &amp;&amp; go vet ./pkg/backup/manifest/... &amp;&amp; go test ./pkg/backup/manifest/... -count=1 -v</automated>
  </verify>
  <done>All 10 manifest tests pass, go vet clean, manifest package builds standalone.</done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: Backupable capability interface + PayloadIDSet + ErrBackupUnsupported</name>
  <files>pkg/metadata/backup.go, pkg/metadata/backup_test.go</files>
  <behavior>
    - New file `pkg/metadata/backup.go` (do NOT modify `store.go` or `errors.go`). Package `metadata`.
    - Exports:
      * `Backupable` interface with exact signature from <interfaces>.
      * `PayloadIDSet` type = `map[string]struct{}` with `NewPayloadIDSet() PayloadIDSet` constructor, `Add(id string)`, `Contains(id string) bool`, `Len() int` methods.
      * `ErrBackupUnsupported` sentinel — `var ErrBackupUnsupported = errors.New("backup not supported by this metadata store")`.
    - Package-level doc comment at top of the file explaining the capability-via-type-assertion pattern (quoted in PATTERNS.md §`pkg/metadata/backup.go`).
    - Tests in `backup_test.go`:
      * `TestNewPayloadIDSet` — constructor returns a non-nil, zero-length set.
      * `TestPayloadIDSetRoundTrip` — Add 3 distinct IDs; Contains returns true for each; Contains returns false for a missing ID; Len == 3.
      * `TestPayloadIDSetDedup` — Add same ID twice; Len == 1.
      * `TestPayloadIDSetNilSafety` — `var s PayloadIDSet` (nil map); `s.Contains("x") == false` (must not panic); `s.Len() == 0`. (Note: `Add` on nil map DOES panic — that's fine, callers use `NewPayloadIDSet()`.)
      * `TestErrBackupUnsupportedIs` — `errors.Is(ErrBackupUnsupported, ErrBackupUnsupported) == true`.
      * `TestBackupableInterfaceShape` — compile-time assertion via `var _ Backupable = (*stubBackupable)(nil)` where `stubBackupable` is a test-file-local minimal impl (returns nil, nil). This test's purpose is to freeze the interface signature — any future drift breaks this test first.
  </behavior>
  <action>
    1. Create `pkg/metadata/backup.go`. Imports: context, errors, io. Only three public symbols; keep file under 80 lines.
    2. `NewPayloadIDSet()` returns `make(PayloadIDSet)`. Method receivers on the slice type (value, not pointer) since it's a map.
    3. Create `pkg/metadata/backup_test.go`. Use `package metadata` (same package, for clean access) or `package metadata_test` (per project convention in this directory — check `store_test.go` or similar to match). Standard testify pattern.
    4. `stubBackupable` test type signature MUST match the interface exactly — this is the purpose of `TestBackupableInterfaceShape`. If it fails to compile, the interface drifted.
    5. NO changes to `memory`, `badger`, or `postgres` metadata store packages. Those get Backupable implementations in Phase 2.
    6. Verify no existing `Backupable` or `PayloadIDSet` identifier exists in pkg/metadata before creating (grep first).
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-368 &amp;&amp; go build ./pkg/metadata/... &amp;&amp; go vet ./pkg/metadata/... &amp;&amp; go test ./pkg/metadata/... -run 'TestPayloadIDSet|TestErrBackupUnsupportedIs|TestBackupableInterfaceShape|TestNewPayloadIDSet' -count=1 -v</automated>
  </verify>
  <done>All 6 backup_test.go tests pass, package builds, `Backupable` interface is visible from `pkg/metadata` so Phase 2 drivers can implement it.</done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Manifest reader (future Phase 5 restore path) ← manifest on disk/S3 | Untrusted bytes from external storage — must not crash the restore path |
| Future Phase 2 Backupable implementations | Must not leak blocks by returning incomplete PayloadIDSet (enforced by tests in Phase 2, not here) |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-01-09 | Tampering | Manifest file on destination bears no signature | accept | v0.13.0 scope: SHA-256 checksum in manifest is verified by restore path (Phase 5). HMAC signing deferred to milestone follow-up per CONTEXT.md. Explicit decision, documented here. |
| T-01-10 | Denial of service | Manifest.Parse with gigabyte YAML input | mitigate | ReadFrom uses `io.ReadAll` — risk if caller passes unbounded reader. Phase 5 restore path must wrap with `io.LimitReader` (note in this plan's SUMMARY for downstream). Phase 1 tests use small fixtures. |
| T-01-11 | Information disclosure | Manifest.Encryption.KeyRef stores sensitive path | accept | KeyRef is an env var name or file path, not a secret. Operator-controlled. Same risk class as existing `BlockStoreConfig.Config`. |
| T-01-12 | Tampering | Future manifest v2 with extra fields parsed as v1 | mitigate | Strict version guard: `ManifestVersion != CurrentVersion` rejected. Future v2 parsers add explicit branching; unknown fields silently dropped by yaml.v3 (default behavior acceptable for additive schema evolution). |
| T-01-13 | Spoofing | A malicious metadata store that fakes Backupable | accept | Capability via type assertion, not runtime registry. Compile-time binding; attackers would need to ship a malicious metadata store binary, which is outside the threat model. |
</threat_model>

<verification>
- `go build ./pkg/backup/manifest/... ./pkg/metadata/...` passes
- `go vet ./pkg/backup/manifest/... ./pkg/metadata/...` clean
- `go test ./pkg/backup/manifest/... ./pkg/metadata/... -count=1` passes (all new tests)
- `grep -c "payload_id_set" pkg/backup/manifest/manifest.go` ≥ 1 (field present)
- `grep -c "omitempty" pkg/backup/manifest/manifest.go` does NOT include the `PayloadIDSet` line
- `grep -c "Backupable" pkg/metadata/backup.go` ≥ 1
- `grep -c "ErrBackupUnsupported" pkg/metadata/backup.go` ≥ 1
- Existing tests still pass: `go test ./pkg/metadata/... -count=1`
</verification>

<success_criteria>
- Manifest v1 exists with all 10 fields, YAML round-trip works, version guard rejects 0 and future versions
- `payload_id_set` field appears in output even when empty (critical for Phase 5 GC hold — SAFETY-01 forward compat)
- Marshal determinism verified (reproducible SHA-256)
- `Backupable` interface compiles with locked stream-based signature (ENG-04)
- `PayloadIDSet` type with Add/Contains/Len works; nil-read safety for Contains/Len
- `ErrBackupUnsupported` sentinel exported
- No metadata store implementation changed in this plan (Phase 2 scope)
- All new package tests pass, existing tests in pkg/metadata/... still green
</success_criteria>

<output>
After completion, create `.planning/phases/01-foundations-models-manifest-capability-interface/01-03-SUMMARY.md` with:
- Files created
- Full Manifest struct definition (final shape after any YAML-tag adjustments)
- Backupable signature and PayloadIDSet type
- Note to downstream: "Manifest.ReadFrom uses io.ReadAll — Phase 5 restore path MUST wrap with io.LimitReader for untrusted storage" (ref T-01-10)
- Confirmation that no existing metadata store (memory/badger/postgres) was modified
</output>
