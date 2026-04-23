---
phase: 08-pre-refactor-cleanup-a0
plan: 15
subsystem: blockstore
tags: [td-04, parser, cleanup, lint-sweep, docs-fix]
requires: [08-14]
provides: [canonical ParseBlockID, updated TD-04 wording]
affects:
  - pkg/blockstore/types.go
  - pkg/blockstore/types_test.go
  - pkg/blockstore/engine/syncer.go
  - pkg/blockstore/engine/gc.go
  - pkg/blockstore/engine/gc_test.go
  - pkg/blockstore/local/fs/recovery.go
  - pkg/blockstore/local/fs/manage.go
  - .planning/REQUIREMENTS.md
  - .planning/ROADMAP.md
tech-stack:
  added: []
  patterns:
    - "ParseStoreKey + ParseBlockID as the two canonical block-key parsers (D-13, D-15)"
key-files:
  created:
    - pkg/blockstore/types_test.go
  modified:
    - pkg/blockstore/types.go
    - pkg/blockstore/engine/syncer.go
    - pkg/blockstore/engine/gc.go
    - pkg/blockstore/engine/gc_test.go
    - pkg/blockstore/local/fs/recovery.go
    - pkg/blockstore/local/fs/manage.go
    - .planning/REQUIREMENTS.md
    - .planning/ROADMAP.md
decisions:
  - "ParseBlockID signature returns (payloadID, blockIdx, err error) — explicit error paths per plan interface + T-08-15-01 mitigation; callers upgraded from silent zero-value fallback to structured parseErr handling."
  - "ParseStoreKey kept with its existing (payloadID, blockIdx, ok bool) signature; symmetry added via TestParseStoreKey_RoundTrip in types_test.go."
  - "Removed gc_test.go's TestParsePayloadIDFromBlockKey (tested the deleted duplicate); coverage for the equivalent format lives in the new TestParseStoreKey_RoundTrip in types_test.go."
metrics:
  duration: "~15 minutes (single atomic commit)"
  completed: 2026-04-23
  tasks: 1
  files_modified: 8
  files_created: 1
---

# Phase 08 Plan 15: TD-04 parser collapse 5 to 2 Summary

Consolidated block-key parsers from 5 to 2 canonical parsers (`ParseStoreKey` for external `{payloadID}/block-{N}` format + `ParseBlockID` for internal `{payloadID}/{blockIdx}` format) per D-13, corrected the "4 → 1" wording in REQUIREMENTS.md and ROADMAP.md to "5 → 2", and confirmed a clean `go vet ./pkg/blockstore/...` sweep (D-20).

## Outcome

- **5 parsers → 2** as required by D-13: `ParseStoreKey` (types.go, kept) + `ParseBlockID` (types.go, new). Four duplicate parsers deleted (`parseStoreKeyBlockIdx`, `parsePayloadIDFromBlockKey`, `parseBlockID`, `extractBlockIdx`). Final grep of the four names across `pkg/**/*.go` returns **0 matches**.
- **W12 satisfied**: `pkg/blockstore/types_test.go` did not exist before this plan; created in step 0 before RED tests were written.
- **W17 satisfied**: both REQUIREMENTS.md line 92 and ROADMAP.md line 47 now contain the explicit "5 → 2" / "Five...two" wording (grep-anchored below).
- **Build + tests**: `go build ./...` clean, `go vet ./pkg/blockstore/...` clean, `go test -count=1 -short -race ./pkg/blockstore/...` + `./pkg/controlplane/...` both pass.

## Commits

| Task | Name | Commit | Files |
|------|------|--------|-------|
| 1 | TD-04 parser collapse + lint sweep + doc correction | `7cff2563` | 9 files (5 .go + 1 new _test.go + 1 _test.go edit + 2 planning .md) |

Commit message: `blockstore: collapse block-key parsers 5 to 2 (TD-04)`
Signature: Good (RSA key `SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8`).
Claude-Code / Co-Authored-By hygiene: PASS (no such strings in the commit body).

## Verification Evidence

### Duplicate parsers grep (expect 0)

```text
$ grep -rn "parseStoreKeyBlockIdx\|parsePayloadIDFromBlockKey\|parseBlockID\b\|extractBlockIdx" pkg/ --include='*.go'
(no output)
```

### Canonical parsers in `pkg/blockstore/types.go`

```text
181:func ParseStoreKey(storeKey string) (payloadID string, blockIdx uint64, ok bool) {
203:func ParseBlockID(blockID string) (payloadID string, blockIdx uint64, err error) {
```

### Test coverage

```text
$ grep -cE "^func TestParseBlockID_RoundTrip|^func TestParseBlockID_Invalid" pkg/blockstore/types_test.go
2
$ grep -cE "^func TestParseStoreKey_RoundTrip" pkg/blockstore/types_test.go
1
$ go test -count=1 -short -race -run 'ParseBlockID|ParseStoreKey' ./pkg/blockstore/...
ok  	github.com/marmos91/dittofs/pkg/blockstore	1.2s
```

### Build + vet + race tests

```text
$ go build ./...
(clean)
$ go vet ./pkg/blockstore/...
(clean)
$ go test -count=1 -short -race ./pkg/blockstore/...
ok  	github.com/marmos91/dittofs/pkg/blockstore	1.239s
ok  	github.com/marmos91/dittofs/pkg/blockstore/engine	6.129s
ok  	github.com/marmos91/dittofs/pkg/blockstore/local/fs	7.142s
ok  	github.com/marmos91/dittofs/pkg/blockstore/local/memory	1.365s
ok  	github.com/marmos91/dittofs/pkg/blockstore/remote/memory	1.768s
ok  	github.com/marmos91/dittofs/pkg/blockstore/remote/s3	1.951s
$ go test -count=1 -short -race ./pkg/controlplane/...
ok  	github.com/marmos91/dittofs/pkg/controlplane/api	2.791s
ok  	github.com/marmos91/dittofs/pkg/controlplane/models	4.042s
ok  	github.com/marmos91/dittofs/pkg/controlplane/runtime	4.379s
ok  	github.com/marmos91/dittofs/pkg/controlplane/runtime/blockstoreprobe	1.768s
ok  	github.com/marmos91/dittofs/pkg/controlplane/runtime/clients	4.090s
ok  	github.com/marmos91/dittofs/pkg/controlplane/runtime/shares	2.955s
ok  	github.com/marmos91/dittofs/pkg/controlplane/runtime/stores	1.452s
ok  	github.com/marmos91/dittofs/pkg/controlplane/store	2.671s
```

### W17 doc-wording anchor (REQUIREMENTS.md line 92)

```text
- [ ] **TD-04**: Five block-key parsers collapsed to two canonical parsers (`ParseStoreKey` for `{payloadID}/block-{N}` + `ParseBlockID` for `{payloadID}/{blockIdx}`; `ParseCASKey` added in A2/Phase 11 per BSCAS-01). Net: 5 → 2.
```

### W17 doc-wording anchor (ROADMAP.md line 47)

```text
  4. Five block-key parsers (`ParseStoreKey`, `parseStoreKeyBlockIdx`, `parseBlockID`, `extractBlockIdx`, `parsePayloadIDFromBlockKey`) collapsed to two canonical parsers: `ParseStoreKey` for `{payloadID}/block-{N}` + `ParseBlockID` for `{payloadID}/{blockIdx}` (net: 5 → 2)
```

### W12 (types_test.go safety net)

Before the plan: `test -f pkg/blockstore/types_test.go` → MISSING.
After step 0 (`printf "package blockstore\n" > pkg/blockstore/types_test.go`): file present.
After step 2 (RED tests written): compile failure on missing `ParseBlockID` confirmed RED before GREEN.

### Lint sweep (D-20)

```text
$ go vet ./pkg/blockstore/...
(clean — exit 0)
$ gofmt -l pkg/blockstore/
(clean — no files flagged)
```

staticcheck was not invoked (not in project tooling path for this worktree); `go vet` is the canonical gate per D-20.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed stale test `TestParsePayloadIDFromBlockKey` in `pkg/blockstore/engine/gc_test.go`.**
- **Found during:** Task 1, step 4.
- **Issue:** The test exercised the `parsePayloadIDFromBlockKey` function that the plan explicitly deletes. Leaving it would cause the package to fail to compile.
- **Fix:** Deleted the test block (lines 17-80 of the original file); equivalent coverage for the `{payloadID}/block-{N}` format lives in the new `TestParseStoreKey_RoundTrip` added to `pkg/blockstore/types_test.go` (covers "simple key", "nested path", "high block index", "missing marker", "non-integer index", "empty string", "marker at index 0" — superset of the deleted test cases).
- **Files modified:** `pkg/blockstore/engine/gc_test.go`.
- **Commit:** `7cff2563` (bundled with the rest of TD-04).

**2. [Rule 3 - Blocking] Removed unused `strconv` import from `pkg/blockstore/local/fs/recovery.go` and `pkg/blockstore/local/fs/manage.go` after deleting the local parsers.**
- **Found during:** Task 1, step 4 post-deletion compile.
- **Issue:** Go `unused import` compile error.
- **Fix:** Dropped `"strconv"` from the import blocks.
- **Files modified:** `pkg/blockstore/local/fs/recovery.go`, `pkg/blockstore/local/fs/manage.go`.
- **Commit:** `7cff2563`.

### Planner-discretion calls

- **ParseBlockID signature:** The plan's interface sketch specified `(payloadID, blockIdx, err error)` and the plan instructed "match ParseStoreKey's style" while also requiring wrapped errors. I interpreted "style" as code idioms (`strings.LastIndex`, `strconv.ParseUint`, wrapped `fmt.Errorf` with `%w`) rather than return signature, keeping the explicit `err error` return as stated in the interface. Both `T-08-15-01` and the plan-mandated `TestParseBlockID_Invalid` cases (missing slash, non-integer idx, empty string, trailing slash, negative idx, lone slash) now produce errors.
- **Error sentinel choice:** Chose `ErrInvalidPayloadID` (defined in `pkg/blockstore/errors.go`) as the base sentinel for the two format-level failures (missing separator, empty idx). The numeric-parse failure unwraps to the underlying `strconv.NumError` — standard Go idiom, and existing `errors.Is(err, ...)` contracts still hold.

### Unchanged

- No architectural changes (Rule 4 not triggered).
- Threat model mitigations (T-08-15-01) are implemented via the table-driven `TestParseBlockID_Invalid` suite; T-08-15-03 (doc drift) mitigated in the same commit with the W17-anchored wording.
- No auth gates.
- No stubs introduced.

## Known Stubs

None.

## Threat Flags

None. The consolidation did not introduce new network endpoints, auth paths, file access patterns, or trust boundaries beyond the plan's declared scope.

## Self-Check

Created files verified:
- `pkg/blockstore/types_test.go` — FOUND.

Modified files verified (all present in commit `7cff2563`):
- `pkg/blockstore/types.go` — FOUND (contains `func ParseBlockID` at line 203, `func ParseStoreKey` at line 181).
- `pkg/blockstore/engine/syncer.go` — FOUND (no `parseStoreKeyBlockIdx`, two call sites using `blockstore.ParseStoreKey`).
- `pkg/blockstore/engine/gc.go` — FOUND (no `parsePayloadIDFromBlockKey`, call site using `blockstore.ParseStoreKey`).
- `pkg/blockstore/engine/gc_test.go` — FOUND (no `TestParsePayloadIDFromBlockKey`).
- `pkg/blockstore/local/fs/recovery.go` — FOUND (no `parseBlockID`, caller using `blockstore.ParseBlockID`, no stale `strconv` import).
- `pkg/blockstore/local/fs/manage.go` — FOUND (no `extractBlockIdx`, two callers using `blockstore.ParseBlockID`, no stale `strconv` import).
- `.planning/REQUIREMENTS.md` — FOUND (TD-04 at line 92 contains "Five...two" + "5 → 2").
- `.planning/ROADMAP.md` — FOUND (Phase 08 Success Criteria point 4 at line 47 contains "Five...two" + "5 → 2").

Commit verified:
- `7cff2563` — FOUND (`git log --oneline --all | grep -q 7cff2563` exits 0).

## Self-Check: PASSED
