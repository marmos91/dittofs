I'll synthesize the REVIEW.md from the verified findings data. Let me note the key triage decision: the ux-coherence status-exit-0 HIGH was adversarially downgraded to MED by the verifier, while the snapshot-failed-exit-0 HIGH was confirmed as HIGH.

# CLI Area Audit — REVIEW.md

**Status:** Wave-1 PR-A audit complete
**Date:** 2026-06-01
**Scope:** `cmd/dfs/` (server CLI), `cmd/dfsctl/` (REST client CLI), `internal/cli/` (shared plumbing) — ~14.7K LOC, two Cobra binaries (`dfs`, `dfsctl`).
**Cross-check refs:** Cobra idioms, UX coherence, `docs/CLI.md` reference surface, repo conventions (less-is-more, delete-eagerly, no plan/phase IDs in source). Default NFS port 12049.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---:|---:|---:|---:|
| dfs-commands | 0 | 0 | 4 | 0 |
| dfsctl-commands | 1 | 2 | 2 | 0 |
| ux-coherence | 0\* | 4 | 2 | 0 |
| security | 0 | 2 | 1 | 0 |
| internal-cli | 0 | 1 | 6 | 0 |
| bloat-simplicity | 0 | 1 | 6 | 0 |
| **Total (post-verification)** | **1** | **10** | **21** | **0** |

\* ux-coherence reported 1 HIGH (`status` always exits 0); adversarial verification **confirmed the bug is real but downgraded it to MED** (CLI/monitoring footgun, not a data-integrity / security / protocol defect). See §3.

**Verdict: PATCH-grade.** Exactly one HIGH survives verification — `dfsctl share snapshot create -o json/yaml` returns exit 0 on a server-reported `failed` snapshot, a silent-success-on-failure in the documented machine-readable mode. It is a contained, well-localized fix. No command corrupts state, loses data, leaks a secret to stdout, defaults TLS to insecure, or silently no-ops a destructive operation. The remaining issues are UX coherence, secret-handling hardening, and modest dead-code/dedup cleanup.

**Architecture invariants hold.** Protocol/business-logic separation is not a CLI concern; the CLI correctly routes through the REST API (dfsctl) and the Runtime (dfs). Error wiring is sound in both binaries (RunE → main → `os.Exit(1)` under `SilenceErrors`/`SilenceUsage`). Destructive-command guarding is uniform. The shared `internal/cli/health` package is consumed identically by both binaries with no divergent reimplementation, and is concurrency-safe with no goroutine leak.

---

## 2. HIGH findings

### H1 — Failed snapshot returns exit 0 under `-o json`/`-o yaml` (silent success on failure)
**`cmd/dfsctl/commands/share/snapshot/create.go:122-143`**

- **What:** In the blocking create path, after `WaitForSnapshot` returns the terminal snapshot, the code switches on output format *first*. `FormatJSON` (lines 123-124) and `FormatYAML` (lines 125-126) call `output.PrintJSON`/`PrintYAML` and `return nil` unconditionally, regardless of `snap.State`. Only the default (table) branch (lines 127-142) inspects `snap.State` and returns `fmt.Errorf("snapshot failed")` (line 138). `internal/cli/output/json.go:9-13` confirms `PrintJSON` returns only the encoder error (nil on a successful write), so a server-reported `state=="failed"` snapshot encodes fine → `runCreate` returns nil → cobra `RunE` exits 0.
- **Why:** A failed server-side snapshot is reported as **success** to any caller using `-o json` — the documented machine-readable mode. CI pipelines and automation that gate on exit code treat a failed backup as a successful one. This is the silent-success-on-server-error class. The blocking failed-state path is untested (`create_test.go` has only `TestCreate_NoWaitJSONHasIDField`; grep for `failed`/`WaitForSnapshot`/`State` in that file returns nothing).
- **Fix:** Inspect `snap.State` independently of output format. Emit the JSON/YAML body for observability, then return a non-nil error when failed:
  ```go
  // print the record in the requested format, then:
  if snap.State == "failed" {
      return fmt.Errorf("snapshot %s failed: %s", snap.ID, snap.Error)
  }
  ```
- **Verifier rationale:** Confirmed real (HIGH) by direct read of `create.go:122-143` and `output/json.go:9-13`. No upstream guard exists — `runCreate` is the cobra `RunE` and nothing re-checks state. Every element independently verified.

---

## 3. Triage downgrades / RESOLVED

### `dfs status` / `dfsctl status` always exit 0 when server is down/unhealthy — **HIGH → MED**
**`cmd/dfs/commands/status.go:181` (return nil); `cmd/dfsctl/commands/status.go:139` (return nil)**

- **Finding (real):** `runStatus` in both binaries computes Running/Healthy/unreachable and prints distinct failure messages, but unconditionally `return nil`. `dfsctl status` sets `status.Status="unreachable"` (line 90) / `status.Error=err` (line 100) yet still returns nil. A scripted `dfs status && echo up` or `dfsctl status || alert` reports success for a stopped/unhealthy/unreachable server. The early `return fmt.Errorf(...)` paths (lines 72/77/82) cover only not-logged-in/config errors, not server health.
- **Verifier verdict — downgrade to MED:** The bug is confirmed by direct code read, but the verifier reclassified it: this is a CLI/monitoring UX footgun, **not** a data-integrity, security, or protocol-correctness defect. JSON/YAML output still exposes the true state to scripts that parse rather than rely on the exit code; there is no silent data loss or auth impact. (Note: the original "docs precedent" citation was partly mis-cited — `docs/CLI.md` migrate-status documents exit 0 regardless of migration *state* but non-zero on network/unknown-share error, which actually *reinforces* that unreachability should be non-zero — i.e. the convention `status` violates.) Carried forward as **MED** under §4 (ux-coherence).

No other HIGHs were proposed by the remaining sub-audits; nothing else required refutation.

---

## 4. MED findings

**ux-coherence / status exit codes**
- **`status` exit-0-on-failure** (`cmd/dfs/commands/status.go:181`, `cmd/dfsctl/commands/status.go:139`) — downgraded from HIGH (see §3). Return non-zero when `!status.Running` (dfs) or `status.Status=="unreachable"`/`!status.Healthy` (dfsctl), keeping table/JSON output intact; document the codes in `docs/CLI.md` to match the migrate/snapshot pattern.

**ux-coherence / convention drift**
- **Snapshot commands break the `--force`=skip-confirm convention** (`cmd/dfsctl/commands/share/snapshot/delete.go:28`, `restore.go:43-44`) — every other destructive command (12+ sites) uses `--force`/`-f` = "skip confirmation"; the snapshot subtree alone uses `--yes` to skip, and `restore --force` is overloaded to mean "allow non-durable". `restore`'s `--force` is also `BoolVar` (no `-f` short) unlike all others' `BoolVarP`. Pick one skip-confirm verb CLI-wide; rename the durability flag to e.g. `--allow-non-durable`.
- **`status` tables hardcode ANSI color, ignore `--no-color` / non-TTY** (`cmd/dfsctl/commands/status.go:151-155`, `cmd/dfs/commands/status.go:192-213`) — raw `\033[3Xm` written directly to stdout; `dfsctl` has `--no-color`/`cmdutil.IsColorDisabled()` but `printStatusTable` never consults it, `dfs` has no `--no-color` at all, and there is no TTY check or `NO_COLOR` support anywhere. Piping `dfs status > file` writes literal escapes. (Same root cause as the internal-cli MED below.)
- **Interactive prompts have no non-TTY fallback** (`internal/cli/prompt/password.go:14-22`, `prompt/input.go:31-39`, `cmd/dfsctl/commands/login.go:71-84`) — promptui/readline returns an opaque low-level error on piped stdin instead of "X required: pass --X (no terminal for prompt)". Add a `golang.org/x/term.IsTerminal` check in the prompt helpers.

**security**
- **Passwords accepted via `--password`/`-p` flags** (`cmd/dfsctl/commands/login.go:44`; also `switch_user.go:38`, `user/create.go:55`, `user/password.go:32`, `user/change_password.go:35-36`, `share/mount.go:80`) — cleartext lands in shell history and process argv (`ps aux`, `/proc/<pid>/cmdline`). Every command already defaults to a masked prompt; per less-is-more, drop the flags (or switch to `DFSCTL_PASSWORD` env / `--password-stdin`), and at minimum stop advertising `-p secret` in help examples.
- **Credential save does not re-chmod or atomically rewrite** (`internal/cli/credentials/store.go:156`) — `os.WriteFile(path, data, 0600)` only applies the mode on *create*; a pre-existing looser-perm file is rewritten without tightening, and there is no temp+rename so a mid-write crash truncates all stored tokens/contexts. Write to a sibling 0600 temp, `Chmod`, then `os.Rename`. (Same defect also raised as LOW under internal-cli — consolidate.)

**internal-cli**
- **`--no-color` parsed but ignored by status/health display** (`internal/cli/health/display.go:7-21,38,79-82`) — `colorStatus`/`colorGreen`/etc. and both `printStatusTable`s emit raw ANSI unconditionally; the documented flag silently does nothing for the most visible shared output. Thread a color-enabled bool into `health.PrintEntityStatus` / `printStatusTable`, gated on `cmdutil.IsColorDisabled()` / `NO_COLOR` / isatty, reusing the existing `output.Printer` color flag. (Cross-references the two ux-coherence color MEDs — single shared fix.)

**bloat-simplicity**
- **`SnapshotDefaults`: empty-struct future-knob wired end-to-end for zero effect** (`pkg/controlplane/runtime/runtime.go:744` type, `:748` setter, `:111` field, `:137` init; called `cmd/dfs/commands/start.go:193`) — `type SnapshotDefaults struct{}` takes a mutex, stores nothing, and exposes public API for a non-existent feature. Delete the type, field, init line, `SetSnapshotDefaults`, and the `start.go:193` call (~15 LOC). Re-add when a real knob ships.

---

## 5. LOW findings

**dfs-commands**
- **`dfs config show` prints the JWT signing secret unredacted** (`cmd/dfs/commands/config/show.go:71-72`) — marshals the whole `*config.Config` to YAML/JSON; `ControlPlane.JWT.Secret` (`pkg/controlplane/api/config.go:53`, `yaml:"secret"`, no redaction) is the HMAC key for all API JWTs. Operators paste `config show` into bug reports/Slack/CI logs. Add `MarshalYAML`/`MarshalJSON` on `JWTConfig` emitting `"***"`. (`config schema` is fine — reflects types only.)
- **Control-plane store (`cpStore`) never `Close`d on graceful shutdown** (`cmd/dfs/commands/start.go:104`) — created via `store.New`, never closed by `runStart` or `Runtime.Shutdown` (`runtime.go:195-204` closes only metadata stores + adapters); no clean WAL checkpoint, badger dir LOCK released only by process death. Contrast `migrate.go:50` which defers `cpStore.Close()`. `defer cpStore.Close()` in `runStart`.
- **`migrate-to-cas` PID guard only checks the default PID path** (`cmd/dfs/commands/migrate_to_cas.go:225-226`) — `--pid-file /custom` evades the probe. Mitigated by the badger metadata-dir LOCK at `:155` catching a concurrent default-config server; residual exposure is an exotic split-config only. Document the limitation or add an OS flock on the storage dir.
- **`followLogs` signal goroutine can outlive the watcher loop** (`cmd/dfs/commands/logs.go:177-180`) — `go func(){ <-sigCh; cancel() }()` leaks if `followLogs` returns via the watcher channel rather than ctx. Benign (short-lived foreground CLI). Add `defer signal.Stop(sigCh)` / select on `ctx.Done()`.

**dfsctl-commands**
- **`GetAuthenticatedClient` ignores a lone `--server` or lone `--token`** (`cmd/dfsctl/cmdutil/util.go:62-63`) — the fast path requires *both*; a `--token`-only invocation with no stored context fails "not logged in" despite help text saying each flag "overrides stored credential". The lower block (79-91) already applies overrides independently; drop the both-required short-circuit.
- **Issue-ID references in source comments** (`cmd/dfsctl/commands/share/create.go:169,175`) — `// Refs #514:` / `// Refs #532:` violate the no-IDs-in-source convention. Keep the explanatory text, drop the `#…`.

**ux-coherence**
- **`docs/CLI.md` drift: `dfsctl blockstore …` does not exist** (`docs/CLI.md:309-328`, `361-520`) — docs reference `dfsctl blockstore audit-refcounts` and a `blockstore migrate` tree; real command is `dfsctl store block audit-refcounts` (`store/block/audit.go:18`) and no `migrate` exists under `cmd/dfsctl`. Update docs to shipped paths.
- **`dfs init` help examples invoke the wrong binary name `dittofs`** (`cmd/dfs/commands/init.go:23,26`) — root `Use` is `dfs` and `start` examples use `dfs`, but init examples say `dittofs init`. `version.go` also prints `dittofs <ver>` for the dfs binary. Change examples to `dfs init`.

**security**
- **`--token` flag passes bearer token via argv** (`cmd/dfsctl/commands/root.go:66`) — same shell-history/argv local-disclosure vector as the password flags; tokens are credential-equivalent (shorter-lived). Prefer `DFSCTL_TOKEN` env; if `--token` is kept for CI, note the argv caveat in help.

**internal-cli**
- **Two divergent confirmation mechanisms** (`cmd/dfsctl/cmdutil/util.go:29-43`) — `cmdutil.ConfirmDestructive` (injectable bufio, "Type y to confirm:") vs `prompt.Confirm` (promptui, "Delete X [y/N]"); two wordings, two abort styles for one concept. Converge on the injectable (unit-testable) helper.
- **`prompt.Confirm` never honors `defaultYes=true`; dead empty-input branch** (`internal/cli/prompt/confirm.go:14-44`) — hardcoded `Default:""`; Enter returns `ErrAbort`→`(false,nil)`, so `Confirm(label,true)` renders `[Y/n]` but Enter yields false; the `result==""` branch (37-39) is unreachable. Latent (all callers pass `defaultYes=false`). Set promptui `Default` from `defaultYes`; remove the dead branch.
- **Non-atomic credential/config write** (`internal/cli/credentials/store.go:144-157`) — duplicate of the security MED; an interrupted `os.WriteFile` truncates `config.json` (all contexts + tokens). Temp+rename.
- **Dead/unused shared CLI helpers** (`internal/cli/prompt/select.go:64-128` et al.) — zero non-defining callers: `prompt.SelectIndex`, `MultiSelect`, `ConfirmDanger`, `InputInt`, `InputWithValidation`, `output.Printer.Writer()`; test-only: `output.DefaultPrinter`, `Printer.ColorEnabled`, `PrintJSONCompact`. Also check `InputPort` (`input.go:103`). Delete per less-is-more.
- **`GenerateContextName` is a no-op stub returning `"default"`** (`internal/cli/credentials/store.go:291-294`) — ignores its `serverURL` arg despite a doc comment claiming URL-derived naming; multiple logins collide on one context. Implement or inline-and-delete; fix the comment regardless.
- **`PrintSuccessWithInfo` bypasses the printer writer** (`cmd/dfsctl/cmdutil/util.go:177-187`) — success line via `output.Printer`, info lines via raw `fmt.Println` (line 185); untestable via captured writer. Route info through the same printer.

**bloat-simplicity**
- **`completion.go` duplicated verbatim across both binaries, reinventing Cobra's built-in** (`cmd/dfs/commands/completion.go:1-60`, twin `cmd/dfsctl/commands/completion.go:1-60`) — ~120 LOC reimplementing the framework default; both root.go set `CompletionOptions.DisableDefaultCmd=true` to suppress it. Drop both (use built-in) or factor into `internal/cli`.
- **`version.go` duplicated near-verbatim** (`cmd/dfs/commands/version.go:1-32`, twin `cmd/dfsctl/.../version.go:1-32`; `Version`/`Commit`/`Date` vars also at both `root.go:13-15`/`26-28`) — move command + build vars into `internal/cli/buildinfo`.
- **`gc.interval` dead config knob** (`cmd/dfs/commands/start.go:194-202`, field `pkg/config/config.go:139-145`) — parsed/validated only to emit a startup WARN; no scheduler exists. UX trap. Remove field, validation, and WARN block.
- **`migrate-to-cas` legacy-compat command + boot guard** (`cmd/dfs/commands/migrate_to_cas.go:50-79`, `275-326`; boot guard `start.go:385-409`) — 404 LOC v0.13→v0.16 one-way shim plus a `.cas-migrated-v1` boot guard. delete-eagerly/no-prod-users would normally cut it for v1.0; **counterpoint:** it's a documented operator runbook (`docs/CONFIGURATION.md`), so removal is a product decision, not pure cleanup. (confidence 45)
- **`dfs migrate` largely redundant with auto-migration on `store.New`** (`cmd/dfs/commands/migrate.go:13-60`) — `store.New` already runs AutoMigrate; `dfs start` migrates on every boot. Only unique value is migrate-without-start; the `ListUsers` "verification" adds little. Keep as documented entrypoint or drop. (confidence 40)
- **`bench` command tree exposed as a top-level production `dfsctl` command** (`cmd/dfsctl/commands/bench/bench.go`, registered `root.go:88`; ~510 LOC) — dev/perf tooling in the user CLI; **counterpoint:** load-bearing per `test/e2e/BENCHMARKS.md`. No action unless a leaner v1.0 dfsctl is wanted (build tag / separate binary). (confidence 30)

---

## 6. Verified-correct

**Error wiring & process lifecycle (dfs)**
- All fallible `dfs` subcommands use `RunE`; `main.go:23-26` prints to stderr + `os.Exit(1)`. `version.go` uses `Run` but does no fallible work — acceptable.
- `root.go:30-31` sets `SilenceUsage`/`SilenceErrors`; single error sink is `main.go` (no double-print).
- `start.go` graceful shutdown is correct: SIGINT/SIGTERM via `signal.Notify` (line 261), select on `sigChan` vs `serverDone`, `cancel()` then `<-serverDone`, `signal.Stop` on both branches (267,279).
- Build-version wiring correct: `main.version` → `commands.Version`; `docs/CLI.md` ldflags `-X main.version=…` targets the right symbol.
- Boot-guard legacy-layout path (`handleLoadSharesError` → `exitFn(78)`) runs at `start.go:224` *before* PID write (246) and `rt.Serve` (256), so the `os.Exit` bypassing defers loses no state; covered by `start_test.go`.
- `daemon_unix.go` double-spawn PID lifecycle is consistent; stale PID files cleared on next start.
- Flag surface matches `docs/CLI.md` for start/stop/status/migrate-to-cas/config — no orphaned/undocumented flags.
- `migrate-to-cas` sentinel safety: per-share `runErr` checked (line 195) before aggregate increment; `bs.Close()` per share (194); `.cas-migrated-v1` written by the migrate library only on full success. Concurrency guard effective for the common case via PID probe + badger dir LOCK (`:155`).

**Destructive-command guarding & errors (dfsctl)**
- Every destructive command confirms or honors `--force`/`--yes`: share delete, store block/metadata remove, group/netgroup/user/context/idmap delete, client disconnect/evict, client sessions destroy, snapshot delete/restore — all via `RunDeleteWithConfirmation` / `ConfirmDestructive` / `ConfirmWithForce`.
- `main.go:22-25` prints `Error: %v` + `os.Exit(1)`; `root.go` `SilenceErrors`/`SilenceUsage` set.
- Server errors readable, not 200-assumed: `doVia` (`client.go:127-134`) treats `StatusCode>=400` as `*APIError`; `restore.go:84-88` special-cases 412 with a `--force` hint.
- `--json`/`--yaml` honored across all read commands via `PrintOutput`/`PrintResource` or explicit format switches.
- Restore gated correctly (refuses enabled share, `--yes` confirm, surfaces 412). Base HTTP timeout 30s; restore uses 30m. Snapshot partial-ID resolution handles exact/prefix/ambiguous correctly. Grant flag XOR/required validation correct. Password reset reads interactively + confirms + min-length-8.

**Security**
- TLS verification on by default everywhere: `apiclient` uses `http.DefaultTransport`, no `InsecureSkipVerify`, no `tls.Config`, **no `--insecure`/`--skip-verify` flag anywhere**.
- Tokens never printed to stdout/stderr: context list/current expose only a `LoggedIn` bool; token passed only as an `Authorization` header; verbose mode prints only operational counts; apiclient never logs the auth header.
- Credential file created 0600 in a 0700 dir under `XDG_CONFIG_HOME` (`config.json`/`%APPDATA%`). Interactive password entry masked (`*`). Logout (`ClearCurrentContext`) zeroes access/refresh/expiry and re-saves. Token-refresh path refuses to overwrite the stored refresh token with incomplete tokens (`util.go:104`). No path-traversal in config-path resolution.

**Shared plumbing (internal/cli)**
- `health.FetchEntities` is concurrency-safe (per-entity fields written by one goroutine; `ent.Errors` appended under `mu`; `allStores` goroutine-local) with no goroutine leak (`wg.Add(4)`/`Done`/`Wait` balanced + 10s client timeout). Timeout propagates from both status commands.
- Both binaries consume the shared `health` package identically — no divergent reimplementation. Readiness display reflects real state including degraded=healthy semantics, consistent across dfs/dfsctl.
- HTTP error bodies bounded via `io.LimitReader(resp.Body,256)` — no unbounded read DoS. No swallowed errors in output helpers (only terminal-write `Fprint` errors intentionally discarded). `go vet` clean on `./internal/cli/...` and `./cmd/dfsctl/cmdutil/...`. `ConfirmDestructive` bias-to-refuse is correct (empty/non-y → false, EOF tolerated).

**Bloat baseline**
- `internal/cli/health` and `internal/cli/timeutil` are genuinely shared (exactly 2 callers each) — correct dedup. No plan/phase/wave IDs leaked into source (version strings v0.13/v0.15/v0.16 are legitimate release refs). adapter enable/disable are distinct justified commands. `config schema` is lean. Cobra default completion correctly disabled in both root.go (no double-register).

---

## 7. Recommended PR-B shape

**PR-B1 — Fix the HIGH (snapshot exit-code correctness).** Inspect `snap.State` independently of output format in `create.go`; print the body then return a non-nil error on `failed` in all formats. Add a regression test for the blocking `-o json` failed-state path (the gap that hid this). Small, isolated, mergeable first.

**PR-B2 — Status exit codes + color (cross-binary UX coherence).** Make `dfs status` / `dfsctl status` return non-zero on not-running/unhealthy/unreachable; document codes in `docs/CLI.md`. Same PR: route all status/health color through a single color-enabled gate (`cmdutil.IsColorDisabled()` / `NO_COLOR` / isatty), reusing `output.Printer` — closes the three color MEDs (ux-coherence ×2 + internal-cli ×1) in one change. Add `--no-color` to `dfs`.

**PR-B3 — Secret-handling hardening.** Redact `JWT.Secret` in `dfs config show` (LOW but high-leverage); atomic temp+rename + re-chmod in `credentials.Store.save()` (consolidates the security MED + internal-cli LOW); move passwords/token off argv to `--password-stdin`/`DFSCTL_PASSWORD`/`DFSCTL_TOKEN` (or drop the flags), and stop advertising `-p secret`.

**PR-B4 — Convention + non-TTY UX.** Standardize snapshot skip-confirm on `--force`/`-f`, rename durability to `--allow-non-durable`; add TTY detection to the prompt helpers with a clear "X required" non-interactive error.

**PR-B5 — Dead-code/dedup sweep (low-risk).** Delete `SnapshotDefaults` (MED) + `gc.interval` knob; fix `prompt.Confirm` `defaultYes`; delete the zero-caller `prompt`/`output` helpers; inline/implement `GenerateContextName`; converge the two confirmation mechanisms; factor `version.go`/`completion.go` into `internal/cli` (or drop completion for Cobra's built-in); fix `config cpStore.Close()` and the `followLogs` goroutine.

**Defer as issues (product decisions, low confidence):** legacy `migrate-to-cas` removal, `dfs migrate` redundancy, `bench` tree placement — all load-bearing/documented; revisit explicitly at v1.0 cut, not as cleanup. **Defer as docs-only issue:** `docs/CLI.md` `blockstore` → `store block` drift; `dittofs` → `dfs` init examples; strip `#514`/`#532` source comments.

---

## 8. Coverage

**Audited (6 parallel sub-audits):**
- **dfs-commands** — full `cmd/dfs/commands` tree: start/stop/status/init/migrate/migrate-to-cas/logs/version/completion + config subcommands; error wiring, signal handling, graceful shutdown, boot-guard, PID/daemon lifecycle, destructive-op guarding, flag-vs-docs parity.
- **dfsctl-commands** — full `cmd/dfsctl/commands` tree: destructive-command confirmation, error/exit-code surfacing, `APIError` handling, `--json`/`--yaml` consistency, timeouts/hang-resistance, snapshot create/restore/delete flows.
- **ux-coherence** — `--output`/`-o`, print helpers, Ctrl+C abort, `--force` consistency, color/TTY handling, prompt behavior, `docs/CLI.md` cross-check.
- **security** — token-in-output, TLS defaults, credential file perms, secrets-via-argv, config-path resolution, logout/refresh scrubbing.
- **internal-cli** — `internal/cli` (credentials/health/output/prompt/timeutil) + `cmd/dfsctl/cmdutil`: concurrency safety, goroutine leaks, error swallowing, confirmation mechanisms, shared-helper dead code, atomic writes.
- **bloat-simplicity** — `cmd/dfs`, `cmd/dfsctl`, `internal/cli` (~14.7K LOC): dedup opportunities, dead knobs, speculative abstractions, legacy-compat surface, plan/phase-ID leakage.

**Not audited (out of CLI scope; cross-referenced only where the CLI touches them):**
- The REST API server, Runtime, metadata/block stores behind the CLI (covered by their own area audits #1, #4, #5, #6). The `SnapshotDefaults` and `gc.interval` findings touch `pkg/controlplane/runtime` and `pkg/config` only at the CLI call sites.
- The `pkg/.../migrate` library internals (only the CLI command layer + boot guard were assessed).
- End-to-end behavioral testing of commands against a live server (static + test-coverage analysis only).
