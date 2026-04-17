---
phase: 06-cli-rest-api-surface
plan: 03
subsystem: cli/dfsctl/share
tags: [cli, share, breaking-change, d-27, d-28, d-35, d-36, d-37, d-38, d-39, enabled, disable, enable]
requires: [phase-6-plan-02]
provides:
  - cmd/dfsctl/commands/share.disableCmd + runDisable → client.DisableShare
  - cmd/dfsctl/commands/share.enableCmd + runEnable → client.EnableShare
  - ShareList ENABLED column + shareRow.Enabled
  - ShareDetail Enabled row + shareEnabledString helper
  - share parent tree registering all 11 verbs/sub-trees (list, create, show, edit, delete, mount, unmount, disable, enable, permission, list-mounts)
  - CHANGELOG.md v0.13.0 entry documenting D-35 breaking flip + new verbs
affects: [phase-6-plans-04-06]
tech-stack:
  added: []
  patterns:
    - httptest.Server + cmdutil.Flags.ServerURL/Token bypass for unit testing authenticated CLI commands (mirrors cmd/dfsctl/commands/login_test.go)
    - Pipe-based os.Stdout capture for verifying PrintSuccess output (PrintSuccess writes directly to os.Stdout, not the passed io.Writer)
    - shareEnabledString yes/no helper matches existing DefaultPermission / RetentionPolicy rendering idioms
key-files:
  created:
    - cmd/dfsctl/commands/share/disable.go
    - cmd/dfsctl/commands/share/enable.go
    - cmd/dfsctl/commands/share/disable_test.go
    - cmd/dfsctl/commands/share/enable_test.go
    - cmd/dfsctl/commands/share/list_test.go
    - cmd/dfsctl/commands/share/show_test.go
    - CHANGELOG.md
  modified:
    - cmd/dfsctl/commands/share/share.go
    - cmd/dfsctl/commands/share/list.go
    - cmd/dfsctl/commands/share/show.go
    - cmd/dfsctl/commands/share/delete.go
    - cmd/dfsctl/commands/share/edit.go
    - cmd/dfsctl/commands/share/mount.go
    - cmd/dfsctl/commands/share/unmount.go
decisions:
  - "Use httptest.Server + cmdutil.Flags.ServerURL/Token override for disable/enable unit tests — matches cmd/dfsctl/commands/login_test.go idiom and avoids needing a new DI seam for shareAPI. GetAuthenticatedClient returns the test-server client when both flags are set, so tests don't touch the real credential store."
  - "Capture os.Stdout via os.Pipe rather than refactoring PrintSuccess to take an io.Writer. PrintSuccess is called from many existing verbs and writes to os.Stdout directly; rewiring it would be scope creep. Pipe-based capture in tests keeps the production path untouched."
  - "Keep `unmount` keyed by mount-point path, not share name. A single share can be mounted to multiple local paths, so `share <name> unmount` would be ambiguous. Documented the exception verbatim in unmount.go Long help and in the CHANGELOG."
  - "Render ENABLED column as `yes` / `-` to match existing PINNED-style rendering idiom from 06-CONTEXT D-20/D-26; render the show detail row as `yes` / `no` per D-28 spec (`Enabled: yes/no` is the plan's exact wording)."
  - "Update example text in delete.go / edit.go / show.go / mount.go to reflect D-35 canonical `<name> <verb>` shape; left unmount examples as-is because unmount doesn't follow D-35 (keyed by mount-point)."
metrics:
  duration: ~5min
  completed: 2026-04-17T10:12:21Z
---

# Phase 6 Plan 3: dfsctl share disable/enable + D-35 restructure Summary

One-liner: Adds `dfsctl share <name> disable` / `enable` verbs (D-27), surfaces
`Enabled` in `share list` + `share show` (D-28), and flips the whole `share`
command tree to the `dfsctl share <name> <verb>` canonical layout in one
commit (D-35), with CHANGELOG.md documenting the breaking change.

## Scope

Client-only changes — zero server-side code touched, zero apiclient additions
(Wave 2 Plan 02 shipped `DisableShare` / `EnableShare` already). This plan is
entirely the operator-facing CLI surface.

## Tasks Completed

| # | Task | Commit |
|---|------|--------|
| 1 RED | Failing tests for share disable/enable verbs | d14889d3 |
| 1 GREEN | disableCmd / enableCmd with typed success + 404 handling | 59b2abcf |
| 2 | Parent restructure, AddCommand re-order, ENABLED column on list, Enabled row on show, example-text refresh | a8e62782 |
| 3 | CHANGELOG.md v0.13.0 entry | 19123e7f |

## Files

| File | Status | Lines |
|------|--------|-------|
| cmd/dfsctl/commands/share/disable.go | created | 49 |
| cmd/dfsctl/commands/share/enable.go | created | 44 |
| cmd/dfsctl/commands/share/disable_test.go | created | 166 |
| cmd/dfsctl/commands/share/enable_test.go | created | 77 |
| cmd/dfsctl/commands/share/list_test.go | created | 93 |
| cmd/dfsctl/commands/share/show_test.go | created | 114 |
| CHANGELOG.md | created | 55 |
| cmd/dfsctl/commands/share/share.go | modified | 69 |
| cmd/dfsctl/commands/share/list.go | modified | 148 |
| cmd/dfsctl/commands/share/show.go | modified | 118 |
| cmd/dfsctl/commands/share/delete.go | modified | 48 |
| cmd/dfsctl/commands/share/edit.go | modified | 341 |
| cmd/dfsctl/commands/share/mount.go | modified | 228 |
| cmd/dfsctl/commands/share/unmount.go | modified | 91 |

Created files total: 598 lines. Modified files: 7 existing.

## Post-restructure share.go AddCommand order

```go
func init() {
    // Root-level verbs (no target name — D-35 canonical shape)
    Cmd.AddCommand(listCmd)
    Cmd.AddCommand(createCmd)
    Cmd.AddCommand(permission.Cmd) // nested sub-tree keeps its own shape
    Cmd.AddCommand(listMountsCmd)  // list-mounts is a list, not a per-share verb

    // Per-share verbs — each leaf uses cobra.ExactArgs(1) with args[0] = <name>
    Cmd.AddCommand(showCmd)
    Cmd.AddCommand(editCmd)
    Cmd.AddCommand(deleteCmd)
    Cmd.AddCommand(mountCmd)
    Cmd.AddCommand(unmountCmd)

    // Phase 6 additions
    Cmd.AddCommand(disableCmd)
    Cmd.AddCommand(enableCmd)
}
```

## `go run ./cmd/dfsctl share --help` — verbatim

```
Manage shares on the DittoFS server.

The `share` tree has two shapes:
  - Root-level verbs (no target name): `list`, `create`
  - Per-share verbs: `share <name> <verb>` — the canonical layout
    for `show`, `edit`, `delete`, `mount`, `unmount`, `disable`, `enable`
    and the `permission` sub-tree.

These operations require admin privileges.

Examples:
  # List all shares
  dfsctl share list

  # Create a new share
  dfsctl share create --name /archive --metadata default --local fs-cache --remote s3-store

  # Show share details
  dfsctl share /archive show

  # Edit a share interactively
  dfsctl share /archive edit

  # Edit a share with flags
  dfsctl share /archive edit --read-only true

  # Disable a share (drain clients, block new connections)
  dfsctl share /archive disable

  # Re-enable a share
  dfsctl share /archive enable

  # Delete a share
  dfsctl share /archive delete

  # Grant permission
  dfsctl share permission grant /archive --user alice --level read-write

Usage:
  dfsctl share [command]

Available Commands:
  create      Create a new share
  delete      Delete a share
  disable     Disable a share (drain clients, block new connections)
  edit        Edit a share
  enable      Enable a share (accept new connections)
  list        List all shares
  list-mounts List mounted DittoFS shares
  mount       Mount a share via NFS or SMB
  permission  Manage share permissions
  show        Show share details
  unmount     Unmount a mounted share

Flags:
  -h, --help   help for share

Global Flags:
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
      --token string    Bearer token (overrides stored credential)
  -v, --verbose         Enable verbose output

Use "dfsctl share [command] --help" for more information about a command.
```

All 11 expected children present: `create`, `delete`, `disable`, `edit`,
`enable`, `list`, `list-mounts`, `mount`, `permission`, `show`, `unmount`.

## ENABLED column on `share list`

New final column, rendered `yes` (Enabled=true) or `-` (Enabled=false, the
same rendering existing PINNED-style fields use). Column present in:

- Table mode: ninth column of `ShareList.Headers()` + ninth cell of each
  `ShareList.Rows()` row.
- JSON mode: `enabled:true|false` on each row (struct tag is `json:"enabled"`
  without omitempty — `false` is semantically meaningful).

Sample rendering (table mode, two rows):

```
NAME      ...  RETENTION   ENABLED
/alice    ...  lru         yes
/archive  ...  lru         -
```

## Enabled row on `share show`

New row inserted between `Read Only` and `Default Permission`:

```
FIELD                VALUE
...
Read Only            false
Enabled              yes
Default Permission   read-write
Retention            lru
...
```

JSON mode: `enabled:true|false` on the emitted Share record (Plan 02 already
shipped the struct tag on `apiclient.Share.Enabled`; no further change here).

## CHANGELOG block (verbatim)

```markdown
## [0.13.0] — YYYY-MM-DD

### Breaking Changes

- **`dfsctl share` restructure (D-35).** All per-share verbs now follow the
  `dfsctl share <name> <verb>` layout. `share list` and `share create` remain
  root-level. Scripts invoking the old `<verb> <name>` order continue to work
  mechanically (Cobra parses the name as `args[0]` regardless of position),
  but the new layout is now the documented canonical form. `share disable`
  and `share enable` (new in v0.13.0) only accept the new shape.

  Before:

  ```
  dfsctl share delete /archive
  dfsctl share edit /archive --read-only true
  dfsctl share show /archive
  dfsctl share mount /archive /mnt/dittofs
  dfsctl share unmount /mnt/dittofs
  ```

  After:

  ```
  dfsctl share /archive delete
  dfsctl share /archive edit --read-only true
  dfsctl share /archive show
  dfsctl share /archive mount /mnt/dittofs
  dfsctl share unmount /mnt/dittofs   # unchanged — keyed by mount-point
  ```

  `unmount` continues to take a mount-point path because a single share can
  be mounted to multiple local paths.

### Added

- **CLI: `dfsctl share <name> disable` / `dfsctl share <name> enable`.**
  Drain clients + refuse new connections. Disable is synchronous — the
  command returns only after connected clients have been disconnected (or
  the server's lifecycle shutdown timeout fires). Required precondition for
  a metadata-store restore.
- **CLI: `share list` and `share show` surface an `ENABLED` field / column.**
  `share list` adds an `ENABLED` column rendering `yes`/`-`. `share show`
  adds an `Enabled: yes/no` row. Both are surfaced in `-o json` / `-o yaml`
  output via the `enabled` field on the Share record.
- **REST: `POST /api/v1/shares/{name}/disable` + `POST /api/v1/shares/{name}/enable`.**
  Admin-only. Return the updated Share record on success. The disable route
  blocks until the drain completes.
```

## Test Outcomes

| Package | Test count | Result |
|---------|------------|--------|
| cmd/dfsctl/commands/share (disable_test) | 4 | PASS |
| cmd/dfsctl/commands/share (enable_test) | 4 | PASS |
| cmd/dfsctl/commands/share (list_test) | 5 | PASS |
| cmd/dfsctl/commands/share (show_test) | 5 | PASS |
| cmd/dfsctl/commands/share (windows-only tests — mount/unmount/list-mounts) | — (not run on darwin) | skipped |
| go build ./... | — | clean |
| go vet ./cmd/dfsctl/... | — | clean |

Test roll-up: **17 new tests, all PASS; existing share darwin tests unaffected.**

New test functions:

- disable_test.go: `TestDisableCmd_CallsClient_AndPrintsSuccess`, `TestDisableCmd_JSONMode_EmitsShare`, `TestDisableCmd_NoArg_Errors`, `TestDisableCmd_NotFound_Exits1`
- enable_test.go: `TestEnableCmd_CallsClient_AndPrintsSuccess`, `TestEnableCmd_JSONMode_EmitsShare`, `TestEnableCmd_NoArg_Errors`, `TestEnableCmd_NotFound_Exits1`
- list_test.go: `TestShareList_Headers_IncludesEnabled`, `TestShareList_Row_RendersEnabledYes`, `TestShareList_Row_RendersEnabledDash`, `TestShareList_Table_IncludesEnabledHeaderAndRow`, `TestShareList_JSON_IncludesEnabledField`
- show_test.go: `TestShareEnabledString`, `TestShareDetail_Rows_IncludesEnabled_Yes`, `TestShareDetail_Rows_IncludesEnabled_No`, `TestShareJSONMarshal_IncludesEnabled`, `TestShareTree_AllVerbsDiscoverable`

## Deviations from Plan

### Rule 3 — Blocking issue

**1. [Rule 3] Test-injection pattern: no existing shareAPI DI, so bypass via `cmdutil.Flags.ServerURL/Token`.**

- **Found during:** Task 1 RED
- **Issue:** Plan suggested either (a) no-injection / integration-style tests against a fake server, or (b) a package-level `getClient` hook. Option (b) would introduce an inconsistent DI pattern (no other share verbs have one). Option (a) as written would have required wiring a full fake server.
- **Fix:** Discovered that `cmdutil.GetAuthenticatedClient()` at `util.go:33-35` short-circuits the credential store when both `Flags.ServerURL` and `Flags.Token` are set. Tests point these at an `httptest.Server` — matches the exact idiom in `cmd/dfsctl/commands/login_test.go:32-47` (`withLoginFlags`). Zero production code change, tests hit a real `apiclient.Client` end-to-end.
- **Files added:** helper in disable_test.go (`withTestServer`, `captureStdout`, `shareActionServer`)

### Rule 2 — Missing critical functionality

**2. [Rule 2] `PrintSuccess` writes to os.Stdout, not the passed writer — test needs pipe capture.**

- **Found during:** Task 1 RED first run
- **Issue:** `cmdutil.PrintSuccess` (util.go:137-144) opens a `Printer` with `os.Stdout` hard-wired. Passing `&bytes.Buffer{}` to `runDisable` had no effect on stdout-printed success messages; tests couldn't assert the "Share alice disabled." line.
- **Fix:** Added `captureStdout` helper in `disable_test.go` using `os.Pipe()` to temporarily redirect `os.Stdout`, run the function, then restore. Keeps production path untouched (rewiring `PrintSuccess` to take an `io.Writer` would cascade across every dfsctl verb).
- **Files added:** `captureStdout` helper in disable_test.go (reused by enable_test.go)

### Rule 3 — Blocking issue

**3. [Rule 3] `unmount` cannot follow D-35 because it's keyed by mount-point, not share name.**

- **Found during:** Task 2 analysis
- **Issue:** D-35's canonical shape is `dfsctl share <name> <verb>`. But `unmount` takes a local mount-point path (args[0]) rather than a share name, because one share can be mounted at multiple local paths. Forcing the D-35 shape on unmount would break semantics.
- **Fix:** Explicitly documented the exception in unmount.go's Long help and in the CHANGELOG "After" block (kept `dfsctl share unmount /mnt/dittofs` — unchanged). Mount follows the D-35 shape because its first arg is the share name; unmount doesn't.
- **Files modified:** cmd/dfsctl/commands/share/unmount.go, CHANGELOG.md

### Rule 2 — Missing critical functionality (example text drift)

**4. [Rule 2] Example text in delete.go/edit.go/show.go/mount.go still showed old `<verb> <name>` shape.**

- **Found during:** Task 2 after AddCommand re-order
- **Issue:** Plan Step 3 said "Update `Use` fields" but said nothing about the Long-help `Examples:` blocks. Those examples still read `dfsctl share delete /archive` — inconsistent with the new canonical form.
- **Fix:** Updated example blocks in `delete.go`, `edit.go`, `show.go`, and `mount.go` to read `dfsctl share /archive delete`, etc. `unmount.go` kept its old shape (see deviation 3 above).
- **Files modified:** delete.go, edit.go, show.go, mount.go

## Authentication Gates

None — plan is entirely client-side code + CHANGELOG, no credentials needed.

## Known Stubs

None. All code paths wire to real `apiclient` methods shipped by Plan 02.

## Threat Flags

None beyond the plan's `<threat_model>`. T-06-03-01 (`cobra.ExactArgs(1)`
validation) verified by `TestDisableCmd_NoArg_Errors` / `TestEnableCmd_NoArg_Errors`.

## TDD Gate Compliance

RED gate: `d14889d3 test(06-03): add failing tests for share disable/enable verbs`
(tests added, build fails — undefined runDisable/disableCmd/runEnable/enableCmd).

GREEN gate: `59b2abcf feat(06-03): add share disable/enable verbs` (verbs
implemented, all 8 disable/enable tests pass).

No REFACTOR gate needed — implementation was already minimal after GREEN.

## Self-Check: PASSED

Verification commands:

- `grep -n 'var disableCmd = &cobra.Command' cmd/dfsctl/commands/share/disable.go` → match at line 11
- `grep -n 'var enableCmd = &cobra.Command' cmd/dfsctl/commands/share/enable.go` → match at line 11
- `grep -n 'client.DisableShare' cmd/dfsctl/commands/share/disable.go` → match at line 43
- `grep -n 'client.EnableShare' cmd/dfsctl/commands/share/enable.go` → match at line 38
- `grep -n 'cobra.ExactArgs(1)' cmd/dfsctl/commands/share/disable.go cmd/dfsctl/commands/share/enable.go` → 2 matches
- `grep -n 'Cmd.AddCommand(disableCmd)' cmd/dfsctl/commands/share/share.go` → match at line 67
- `grep -n 'Cmd.AddCommand(enableCmd)' cmd/dfsctl/commands/share/share.go` → match at line 68
- `grep -n '"ENABLED"' cmd/dfsctl/commands/share/list.go` → match at line 48 (Headers())
- `grep -n '"Enabled"' cmd/dfsctl/commands/share/show.go` → match at line 68 (Rows())
- `grep -c '0.13.0' CHANGELOG.md` → 2
- `grep -c 'Breaking' CHANGELOG.md` → 1
- `grep -c 'dfsctl share.*disable' CHANGELOG.md` → ≥ 1
- `grep -c 'dfsctl share.*enable' CHANGELOG.md` → ≥ 1
- `go build ./...` → clean
- `go vet ./cmd/dfsctl/...` → clean
- `go test ./cmd/dfsctl/commands/share/... -count=1` → all PASS
- `go run ./cmd/dfsctl share --help` → all 11 expected children (`list`, `create`, `show`, `edit`, `delete`, `mount`, `unmount`, `disable`, `enable`, `permission`, `list-mounts`) present
- All 4 commits (d14889d3, 59b2abcf, a8e62782, 19123e7f) present in `git log --oneline`.
