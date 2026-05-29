---
phase: 25-cli-rest-api-documentation-parallel-with-24
plan: 03
subsystem: cli
tags: [snapshot, apiclient, cobra, dto, restore, confirm]

requires:
  - phase: 25-cli-rest-api-documentation-parallel-with-24
    plan: 01
    provides: dto.Snapshot et al, Runtime.RestoreSnapshot 2-return signature

provides:
  - "apiclient.Snapshot (alias for dto.Snapshot) + 4 sibling DTO aliases"
  - "6 typed apiclient methods: CreateSnapshot, ListSnapshots, GetSnapshot, DeleteSnapshot, RestoreSnapshot, WaitForSnapshot"
  - "WithRestoreTimeout(d time.Duration) functional option (default 30m)"
  - "Client.doWithTimeout — thread-safe per-call timeout override"
  - "cmdutil.ConfirmDestructive(prompt string, yes bool) (bool, error) — Y/N gate with overridable ConfirmInput/ConfirmOutput"
  - "5 cobra leaves under cmd/dfsctl/commands/share/snapshot/: create, list, show, delete, restore"
  - "snapshot.Cmd registered in share.Cmd next to permission.Cmd"

affects:
  - 25-02 REST handlers (consume the same dto.Snapshot wire shape)
  - 25-04 docs (operator-visible flag/command vocabulary fixed here)

tech-stack:
  added: []
  patterns:
    - "Narrow snapshotClient interface in cmd/dfsctl/commands/share/snapshot — getClient seam swappable from tests with a fakeClient"
    - "Type-alias re-export of dto.* in apiclient (no struct duplication)"
    - "Per-call HTTP timeout via cloned http.Client over the same Transport (no mutation of c.httpClient)"

key-files:
  created:
    - pkg/apiclient/snapshots.go
    - pkg/apiclient/snapshots_test.go
    - cmd/dfsctl/commands/share/snapshot/snapshot.go
    - cmd/dfsctl/commands/share/snapshot/client.go
    - cmd/dfsctl/commands/share/snapshot/create.go
    - cmd/dfsctl/commands/share/snapshot/list.go
    - cmd/dfsctl/commands/share/snapshot/show.go
    - cmd/dfsctl/commands/share/snapshot/delete.go
    - cmd/dfsctl/commands/share/snapshot/restore.go
    - cmd/dfsctl/commands/share/snapshot/fake_test.go
    - cmd/dfsctl/commands/share/snapshot/stdout_test.go
    - cmd/dfsctl/commands/share/snapshot/list_test.go
    - cmd/dfsctl/commands/share/snapshot/delete_test.go
    - cmd/dfsctl/commands/share/snapshot/restore_test.go
  modified:
    - pkg/apiclient/client.go (Client + restoreHTTPTimeout, ClientOption, WithRestoreTimeout, doWithTimeout)
    - cmd/dfsctl/cmdutil/util.go (ConfirmDestructive + ConfirmInput / ConfirmOutput)
    - cmd/dfsctl/cmdutil/util_test.go (5 ConfirmDestructive cases)
    - cmd/dfsctl/commands/share/share.go (import + Cmd.AddCommand(snapshot.Cmd))

key-decisions:
  - "apiclient.Snapshot et al are `type Snapshot = dto.Snapshot` aliases — callers can pass dto.Snapshot interchangeably; no parallel struct definitions; grep gates enforce."
  - "Client.doWithTimeout builds a transient http.Client sharing the same Transport/CheckRedirect/Jar so connection pooling holds, and never mutates c.httpClient — concurrent callers are safe."
  - "Default restore timeout is 30 minutes (defaultRestoreHTTPTimeout constant); WithRestoreTimeout option overrides it at construction time."
  - "ConfirmDestructive prompt is biased toward refusal: only `y` / `yes` (case-insensitive) confirm; EOF, empty input, and anything else aborts."
  - "Test-side prompt injection: cmdutil.ConfirmInput / ConfirmOutput are package-level io.Reader/io.Writer vars; tests swap and restore via defer."
  - "CLI leaves use a getClient seam (var getClient = func() (snapshotClient, error)) so unit tests inject a fakeClient that implements the narrow snapshotClient interface. Production path returns cmdutil.GetAuthenticatedClient unchanged."
  - "Restore preflight uses the exact hint string `share <name> is enabled; run 'dfsctl share disable <name>' first` printed to stderr; tests assert the exact string."
  - "Restore 412 (PRECONDITION_FAILED) hint: `Snapshot <id> is not remotely durable. Re-run with --force to restore anyway.` printed to stderr; the leaf exits non-zero."
  - "Safety-snap line is omitted when RestoreSnapshotResponse.SafetySnapshotID is empty (covers precheck-fail path on the handler side)."
  - "List table is 6 columns: ID(8 char) NAME STATE DURABLE CREATED SIZE. SIZE is `-` unless DumpBytes>0 (handler does not populate ManifestCount in list mode)."
  - "Newest-first sort applied AFTER --state / --name-prefix filtering."
  - "applyFilters / truncID / formatCreated / formatSize are unexported helpers tested directly — no full cobra Execute() invocation needed for filter and rendering coverage."

requirements-completed:
  - API-02
  - API-03
  - API-04
  - API-05
  - API-06
  - API-07

# Metrics
duration: ~45min
completed: 2026-05-29
---

# Phase 25 Plan 03: dfsctl share snapshot surface + apiclient typed methods

Shipped the operator-facing CLI tree (`dfsctl share snapshot {create,list,show,delete,restore}`) plus the typed Go client backing it. Wire DTOs are type-aliases of `pkg/controlplane/api/dto`, with no struct duplication. `apiclient.New` now accepts a `WithRestoreTimeout(d)` option so restore calls do not get killed by the 30s default `http.Client` timeout.

## Performance

- **Duration:** ~45 min
- **Tasks completed:** 2 of 3 (Task 3 deferred — see below)
- **Files modified:** 17 (14 created, 3 modified)
- **Commits:** 3 (1 RED test, 2 GREEN feat)

## Task Commits

1. **Task 1 RED — failing apiclient tests** — `bcac8915` (test)
2. **Task 1 GREEN — apiclient DTOs + 6 methods + WithRestoreTimeout** — `1165758c` (feat)
3. **Task 2 — 5 cobra leaves + ConfirmDestructive + share.go wire-up + tests** — `a542c9da` (feat)

## Accomplishments

- `pkg/apiclient/snapshots.go` (124 lines) — 5 DTO type aliases + 6 typed client methods. All built on existing `getResource[T]` / `listResources[T]` / `createResource[T]` / `deleteResource` generic helpers. `RestoreSnapshot` is the only path that uses the long-timeout transport, via `c.doWithTimeout`.
- `pkg/apiclient/client.go` extended with `ClientOption` + `WithRestoreTimeout` + `defaultRestoreHTTPTimeout = 30 * time.Minute` constant + thread-safe `doWithTimeout` helper. `New(baseURL, opts ...ClientOption)` now accepts variadic options; existing one-arg call sites continue to compile because the second arg is variadic.
- `cmd/dfsctl/commands/share/snapshot/` package — parent `Cmd` + 5 leaves mirroring `permission/` structure exactly. Each leaf has flags, RunE, and table/JSON/YAML output dispatch via `cmdutil.GetOutputFormatParsed`.
- `cmdutil.ConfirmDestructive(prompt, yes)` — new shared destructive-confirmation helper. Existing `prompt.Confirm` is promptui-driven (TTY-required); `ConfirmDestructive` reads from overridable `ConfirmInput` / `ConfirmOutput` so unit tests work in CI without a TTY.
- Newest-first sort + state / name-prefix filtering live in `applyFilters` (unexported, unit-tested directly).
- Restore preflight calls `GetShare` and refuses on `Enabled==true` with the exact hint string the plan specifies, printed to stderr. The leaf returns a non-nil error so cobra exit code is non-zero.
- Restore 412 surfacing checks `errors.As(err, &*apiclient.APIError)` for `StatusCode == 412` and prints the `--force` hint to stderr.
- Safety-snap line is rendered from `RestoreSnapshotResponse.SafetySnapshotID` and omitted when empty.

## Exact Shape of New Surfaces

### `WithRestoreTimeout`

```go
type ClientOption func(*Client)

func WithRestoreTimeout(d time.Duration) ClientOption {
    return func(c *Client) { c.restoreHTTPTimeout = d }
}

const defaultRestoreHTTPTimeout = 30 * time.Minute

func New(baseURL string, opts ...ClientOption) *Client {
    c := &Client{
        baseURL:            baseURL,
        httpClient:         &http.Client{Timeout: 30 * time.Second},
        restoreHTTPTimeout: defaultRestoreHTTPTimeout,
    }
    for _, opt := range opts { opt(c) }
    return c
}
```

`WithToken` propagates `restoreHTTPTimeout` to the returned clone.

### `ConfirmDestructive`

```go
var ConfirmInput  io.Reader = os.Stdin
var ConfirmOutput io.Writer = os.Stdout

func ConfirmDestructive(prompt string, yes bool) (bool, error)
```

Bias: only `y` / `yes` (case-insensitive, whitespace-trimmed) returns true. EOF / empty / anything else returns false. Test overrides:

```go
cmdutil.ConfirmInput = strings.NewReader("y\n")
cmdutil.ConfirmOutput = &bytes.Buffer{}
```

### Snapshot list table — exact column widths

The table uses `internal/cli/output.PrintTable`, which delegates to `tablewriter.NewWriter` with `SetTablePadding("  ")` and `SetNoWhiteSpace(true)` — column widths are auto-sized per row. **Logical** column structure:

| ID (8) | NAME | STATE | DURABLE (yes/no) | CREATED (relative or RFC3339) | SIZE |
|--------|------|-------|------------------|-------------------------------|------|

- `ID` is truncated to exactly 8 chars by `truncID`.
- `NAME` is `cmdutil.EmptyOr(s.Name, "-")`.
- `DURABLE` is `cmdutil.BoolToYesNo(s.RemoteDurable)`.
- `CREATED` is the relative form (`30s ago`, `5m ago`, `3h ago`, `2d ago`) unless `--no-relative` is passed, in which case UTC RFC3339 is rendered.
- `SIZE` is the `bytesize.ByteSize(DumpBytes)` string, `-` when `DumpBytes <= 0` (which is the list-mode handler default).

### DTO alias confirmation

```bash
$ grep -n "type Snapshot = dto.Snapshot" pkg/apiclient/snapshots.go
15:type Snapshot = dto.Snapshot
$ grep -c "type Snapshot struct" pkg/apiclient/snapshots.go
0
```

Aliases (no redeclaration) verified.

## Deferred Tasks

### Task 3 — built-binary E2E CLI test (`test/e2e/snapshot/snapshot_cli_test.go`)

**Status:** Deferred. The plan's own `<read_first>` for Task 3 declares the prerequisite explicitly:

> this task assumes Plan 25-02's handler stack exists in the tree. The orchestrator should schedule Task 3 after both 25-02 main tasks and 25-03 Tasks 1–2 have shipped.

This worktree's base is `3bdeacac` (wave 1 tip — i.e. only Plan 25-01 has shipped). Plan 25-02 has not yet built the snapshot REST handlers, router wiring, nor the in-process server stack (`test/e2e/snapshot/main_test.go`, `snapshot_http_test.go`) that Task 3 needs to exec the `dfsctl` binary against. Writing the test now would either:

1. Refer to symbols that don't exist (compile failure on `go build -tags=e2e ./test/e2e/snapshot/`), or
2. Stub the server in this PR — duplicating work that Plan 25-02 is responsible for and defeating the e2e purpose (the test should exercise the real handler stack, not a mock).

**Resolution:** the orchestrator should run Task 3 after Plan 25-02 lands. All the CLI behaviors Task 3 would cover (table/JSON/YAML output, prompts, `--yes`, exit codes, safety-snap line, refuse-on-enabled hint, `--force` flow) are already covered by the unit tests in `cmd/dfsctl/commands/share/snapshot/{list,delete,restore}_test.go` against a fake `snapshotClient`. Task 3 is incremental coverage (real binary, real server), not new behavioral coverage.

## Deviations from Plan

None against Tasks 1 and 2 — both executed as written.

### Note on Task 1 implementation detail

The plan suggests "If `createResource` gates strictly on 200/201, add `createResourceAccepted[T]` next to it for 202." Inspection of `Client.do` shows it accepts any status `< 400` — 202 already round-trips through the existing `createResource[T]` and decodes into the body. No new helper was needed.

### Note on Task 2 client-injection

The plan's prescribed test strategy was "use a fake `apiclient.Client` (interface-based fake or substitute `cmdutil.GetAuthenticatedClient` via test indirection)." I chose the interface-based fake — added a narrow `snapshotClient` interface and a `var getClient = func() (snapshotClient, error)` seam in the snapshot package. Production path calls `cmdutil.GetAuthenticatedClient` unchanged; only the leaf-internal `getClient` is overridable from tests. This avoids mutating the global `cmdutil` package state across the entire CLI under test.

## Issues Encountered

None. All builds and tests passed on first GREEN run after Task 1 RED; Task 2 unit tests passed on first run.

## Verification Output

```
$ go build ./...
(no output — exit 0)

$ go vet ./pkg/apiclient/... ./cmd/dfsctl/...
(no output — exit 0)

$ gofmt -s -l pkg/apiclient/ cmd/dfsctl/
(no output)

$ go test ./pkg/apiclient/ ./cmd/dfsctl/cmdutil/ ./cmd/dfsctl/commands/share/snapshot/ -count=1
ok      github.com/marmos91/dittofs/pkg/apiclient                        0.377s
ok      github.com/marmos91/dittofs/cmd/dfsctl/cmdutil                   0.533s
ok      github.com/marmos91/dittofs/cmd/dfsctl/commands/share/snapshot   0.715s

$ grep -n "type Snapshot struct" pkg/apiclient/snapshots.go
(no output — gate satisfied: no struct redefinition)

$ grep -n "type Snapshot = dto.Snapshot" pkg/apiclient/snapshots.go
15:type Snapshot = dto.Snapshot

$ grep -rEn "D-[0-9]+-[0-9]+|per D-|Phase [0-9]+" \
    pkg/apiclient/snapshots.go cmd/dfsctl/commands/share/snapshot/ cmd/dfsctl/cmdutil/util.go
(no output — gate satisfied: no GSD metadata in source)

$ grep -n "snapshot.Cmd" cmd/dfsctl/commands/share/share.go
54:     Cmd.AddCommand(snapshot.Cmd)

$ grep -cE "func \(c \*Client\) (CreateSnapshot|ListSnapshots|GetSnapshot|DeleteSnapshot|RestoreSnapshot|WaitForSnapshot)" pkg/apiclient/snapshots.go
6
```

## Self-Check: PASSED

- `pkg/apiclient/snapshots.go` — FOUND
- `pkg/apiclient/snapshots_test.go` — FOUND
- `pkg/apiclient/client.go` — FOUND (modified)
- `cmd/dfsctl/commands/share/snapshot/{snapshot,create,list,show,delete,restore,client}.go` — all FOUND
- `cmd/dfsctl/commands/share/snapshot/{list,delete,restore,fake,stdout}_test.go` — all FOUND
- `cmd/dfsctl/cmdutil/util.go` + `util_test.go` — FOUND (modified)
- `cmd/dfsctl/commands/share/share.go` — FOUND (modified)
- Commit `bcac8915` (Task 1 RED) — FOUND in git log
- Commit `1165758c` (Task 1 GREEN) — FOUND in git log
- Commit `a542c9da` (Task 2) — FOUND in git log
- `go build ./...` exit 0
- `go vet ./...` exit 0 on touched packages
- `go test` passes for all three touched packages
- `gofmt -s -l` clean on touched directories
- DTO alias gate: 1 alias, 0 struct redeclarations
- GSD metadata gate: 0 matches in plan's edits
- 6 client method signatures present

## Threat Flags

None — no new network endpoints, auth paths, or trust boundaries introduced beyond what Plan 25-02 will mount server-side. The CLI is a trusted-operator surface; threat model is unchanged from the plan's `<threat_model>` block.

## Next Steps

- **Plan 25-02** can land its REST handler stack referencing the same `dto.Snapshot` wire type without any apiclient changes.
- **Task 3** (built-binary E2E CLI test) should be scheduled after 25-02 lands.
- **Plan 25-04** can use the operator vocabulary as shipped here (`--no-verify`, `--force`, `--yes`, `Safety snap:` line, refuse-on-enabled hint string).

---
*Phase: 25-cli-rest-api-documentation-parallel-with-24*
*Plan: 03*
*Completed: 2026-05-29*
