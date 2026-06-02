# Discarded-error (`_ =`) sampling audit — issue #1009

**Scope.** The tree has ~2360 `_ =` discarded-error sites. This was a *sampling*
audit (not a full sweep) over the non-reserved control-plane / CLI / config /
snapshot packages: `cmd/`, `pkg/config/`, `internal/controlplane/`,
`pkg/controlplane/` (excluding the runtime lock-manager files), `pkg/snapshot/`,
`pkg/api/`. NFS/SMB/blockstore/metadata/auth/e2e were reserved for other agents
and not touched.

**Sample size.** ~55 sites enumerated and individually reviewed (the full
population of discards in the allowed dirs is ~118; every suspicious-looking
call — Marshal/Encode/persist/Write/state-flip — was read, plus a representative
slice of the obviously-benign ones).

## Result

| Outcome | Count (sites) |
|---|---|
| Benign — left untouched | ~50 |
| Genuine swallow — fixed | 5 sites across 3 files / 3 bug classes |

### Fixes

1. **`internal/controlplane/api/handlers/shares.go:437,443`** — share-create
   handler swallowed `store.SetShareAdapterConfig` for the default NFS and SMB
   adapter configs. A persist failure there leaves a newly-created share with no
   default adapter config and *zero diagnostic trail*. Now logged at `Warn`,
   matching the existing "Share created but failed to add to runtime" precedent
   in the same handler. Behavior (request still succeeds) is unchanged.

2. **`pkg/controlplane/runtime/adapters/service.go:97`** — `CreateAdapter`
   rollback (`store.DeleteAdapter`) was discarded. If the rollback delete fails
   after a failed adapter start, the store keeps a config row for an adapter
   that will not start — a silent orphan. Now logged at `Warn`. (The sibling
   `_ = s.stopAdapter(...)` discards at `:123`/`:157` were reviewed and left:
   `stopAdapter` already logs its own stop errors/timeouts internally, and its
   only swallowed signal otherwise is the expected "adapter not running".)

3. **`pkg/controlplane/runtime/trash/reaper.go:54,57`** — the background trash
   reaper swallowed per-share `reapShareAt` / `evictToCap` errors. Swallowing is
   intentional for loop resilience (one bad share must not stall the others),
   but neither callee logs internally, so a share that *persistently* fails to
   reap would grow its trash bin forever with no operator signal. Now logs each
   per-share failure at `Warn` while keeping the loop resilient; doc comment
   updated.

## Benign classes observed (left untouched, by design)

- **Deferred `Close()` on read-only handles / after the real error surfaced** —
  e.g. `defer func() { _ = f.Close() }()`, `_ = resp.Body.Close()`,
  `_ = j.Close()`, `_ = bs.Close()`. The meaningful error already surfaced on
  the read/write path; the close is best-effort.
- **`fmt.Fprintf`/`Fprintln` to stdout/stderr/`cmd.Out`** — write-to-terminal,
  no actionable error.
- **Cobra `MarkFlagRequired("literal")`** — only errors on an unknown flag name,
  which is a compile-time-stable literal; cannot fail at runtime.
- **Cobra `cmd.Flags().GetString/GetBool` in PersistentPreRun** — the flags are
  declared on the same command; lookup cannot fail.
- **`json.NewEncoder(w).Encode(...)` into an `http.ResponseWriter`** — headers
  and status are already written (e.g. RFC 7807 `WriteProblem`); a body-encode
  failure (client disconnect) is unrecoverable. Standard idiom.
- **Best-effort cleanup `os.Remove(tmpPath)` / `os.RemoveAll(dir)`** — atomic-
  write temp-file cleanup and bench/probe scratch dirs.
- **Documented idempotent best-effort ops** — `AddUserToGroup` (duplicate-add
  expected), GORM `DropIndex/DropColumn` migration drops (IF-EXISTS semantics).
- **`viper.BindEnv(key)`** — only errors on an empty key; keys are literal or
  reflected struct paths.
- **Error-path state-flip cleanup** — e.g. snapshot `UpdateSnapshotState(...,
  StateFailed)` where the function already returns a wrapped primary error.
- **CLI interactive `json.Unmarshal(current.Config, &cfg)`** — failure leaves a
  nil map; downstream prompts already guard `if cfg != nil` and just show empty
  defaults. UX degradation only, no state corruption; left as-is to avoid churn.

## Candidate noted, not changed

- **`internal/controlplane/api/handlers/shares.go:799`** — `_ =
  h.runtime.RemoveShare(name)` in the share-delete handler. The discard is
  documented ("Ignore error if not found in runtime") and the common case is
  not-found, but a non-not-found failure (e.g. block-store close error) leaks
  runtime resources while the DB row is deleted. Left unchanged for this
  sampling pass; flagged for a future targeted fix if runtime teardown errors
  become a concern.

## Recommendation on a full sweep for v1.0

**A full mechanical sweep is NOT warranted for v1.0.** The discard population is
overwhelmingly benign and falls into the well-known idiomatic classes above; a
2360-site sweep would be almost entirely churn with a poor signal-to-noise ratio
and a real regression risk. The genuine bugs cluster narrowly around
**persist/rollback/background-loop error paths** — three found here, all in the
control plane.

Targeted, higher-yield follow-ups instead:
- A focused grep for `_ = .*\.(Set|Update|Delete|Create|Save|Put|Write|Persist)`
  across the persist layers (this is where the real swallows live) is a small,
  high-value pass worth doing per-area as those areas are audited.
- Consider an `errcheck` lint gate with an exclude-list for the benign idioms
  (Fprintf-to-terminal, deferred Close, MarkFlagRequired) so *new* swallows on
  persist/state-flip paths are caught at CI time rather than by future audits.
