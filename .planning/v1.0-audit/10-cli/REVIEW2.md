# CLI Area Audit — REVIEW.md (Round 2)

**Status:** Wave-1 ROUND-2 audit complete (missed-findings + integration lens)
**Date:** 2026-06-01
**Scope:** `cmd/dfs/` (server CLI), `cmd/dfsctl/` (REST client CLI), `internal/cli/` (shared plumbing), and the `pkg/apiclient` ↔ server REST error contract — the cross-component seam round-1 audited each side of in isolation. Two Cobra binaries (`dfs`, `dfsctl`).
**Cross-check refs:** Round-1 `10-cli/REVIEW.md` (1 HIGH + 10 MED + 21 LOW; PR-B NOT done — every round-1 finding treated as KNOWN and **not** re-reported). RFC7807 problem+json (server `WriteProblem`), the operator-area H1 (RFC7807 client-decode drift) as the canonical sibling-bug. Default NFS port 12049.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---:|---:|---:|---:|
| error-surfacing | 0 | 2 | 1 | 0 |
| dfsctl-rest-contract | 1 | 0 | 2 | 0 |
| credential-security | 0 | 3 | 0 | 0 |
| cli-cross-binary | 1\* | 1 | 0 | 0 |
| **Total (post-verification, de-duplicated)** | **1** | **5** | **3** | **0** |

\* The `cli-cross-binary` HIGH is a re-verification of round-1 H1 (snapshot create `-o json/yaml` exits 0 on `failed`). It **still reproduces verbatim on current develop** (`create.go:122-143`; PR-B not done) and is counted in the round-1 tally — it is **not** a net-new round-2 HIGH. The one genuinely new round-2 HIGH is the `dfsctl-rest-contract` RFC7807 decode drift. Net-new round-2 totals: **1 HIGH / 5 MED / 3 LOW**.

**Verdict: NEEDS-FIX (still PATCH-grade in blast radius).** Round-2 surfaced one net-new HIGH — a shipped cross-component error-contract break in `pkg/apiclient` (decodes legacy `{code,message}`, server emits RFC7807 `{title,detail}`) — that round-1 missed entirely and even listed as verified-correct in its §6 ("Server errors readable, not 200-assumed"). The headline theme is that **the worst CLI bugs live at boundaries round-1 audited in isolation**: (1) the client/server error shape diverged silently because every apiclient test mocks a shape the server never emits; (2) the exit-0-on-failure class round-1 found in snapshot-create **recurs** in `store block gc` and `store block audit-refcounts` — round-1 audited only one command of a class; (3) credential handling is sound at rest and on the wire but leaks the cleartext SMB password into a *different* attacker-readable child process (`mount.cifs`/`net use`) and has no concurrency story for the shared `config.json`. None of the net-new findings is data-loss/auth-bypass/corruption — they are usability/contract-integrity, local-disclosure, and concurrency-integrity class. All fixes are localized and PATCH-grade.

**Architecture invariants hold.** Protocol/business-logic separation is intact; the CLI routes through the REST API and Runtime as before. The boundary defects are contract/serialization drift, not invariant violations.

---

## 2. HIGH findings

### H1 — `dfsctl` apiclient decodes legacy `{code,message}` but the server emits RFC7807 `{title,detail}` — every CLI error surfaces a raw JSON blob, and `IsConflict`/`IsAuthError`/`IsValidationError` are dead
**`pkg/apiclient/client.go:126-132`; `pkg/apiclient/errors.go:9-13,24-41`; `internal/controlplane/api/handlers/problem.go:11-53`**

- **What:** The server's *sole* error path is `WriteProblem` (`problem.go:42-53`, 443 call sites) emitting `application/problem+json` with `{type,title,status,detail,code,hint}`. It **never sets `message`**, and the standard helpers (`BadRequest`/`Unauthorized`/`Forbidden`/`NotFound`/`Conflict`/`UnprocessableEntity`/`PreconditionFailed`/`InternalServerError`/…, `problem.go:57-111`) **never set `code`** — the #414 taxonomy field at `problem.go:30` is defined but unpopulated (grep for `Code:` across non-test handlers returns nothing; the only `json:"message"` in handlers is `grace.go:46`, a 200-OK success body). The client decodes into `APIError{json:"code"; json:"message"; json:"details"}` (`errors.go:9-13`). Three consequences:
  1. **Raw JSON blob as the user-facing message.** `client.go:128` gates the structured-error path on `apiErr.Message != ""`; since the server never sends `message`, `Message` is always empty → control falls to `client.go:132` which sets `Message = string(respBody)` (the entire RFC7807 document). A login failure (`auth.go:82 Unauthorized(w,"Invalid username or password")`) reaches the user as the literal string `{"type":"about:blank","title":"Unauthorized","status":401,"detail":"Invalid username or password"}` instead of the message — leaking the internal problem schema.
  2. **Dead error-classification branches.** `Code` is never populated, so `IsAuthError`/`IsConflict`/`IsValidationError` (`errors.go:24-41`) — which gate *only* on `Code` with no `StatusCode` fallback — always return `false`. Only `IsNotFound` survives (it has a `StatusCode==404` fallback) plus the explicit 412 path. Concretely dead on current develop: `netgroup/delete.go:50-55` (the `IsConflict()` "Shares using this netgroup" friendly message + `Details` follow-up never fires); `adapter/enable.go:67` (the `IsConflict()` create-create race-recovery never triggers → a benign race becomes a hard error).
  3. **`Details` always empty.** Server tags `detail` (`problem.go:23`); client reads `details` (`errors.go:11`) — tag mismatch.
- **Why:** This is the exact cross-boundary error-shape drift class that motivated the operator-area H1, present in the **sibling dfsctl client** and missed by round-1, which listed "Server errors readable, not 200-assumed" as verified-correct in §6 citing only the 412 `StatusCode` path. It is invisible to the test suite because **every** apiclient error test mocks the legacy `{code,message}` shape the server never produces (`client_test.go:73`, `snapshots_test.go:132/166`, `users_test.go:64/123`, `auth_test.go:49/101`), and the one test that sends real problem+json (`blockstore_migrate_status_test.go:60-77`) asserts only `IsNotFound()`/`StatusCode`, never that `Message` is populated. A shipped, cross-component contract break: unreadable raw-JSON errors for **all** 4xx/5xx, wrong/dead error classification, schema leak. No data loss.
- **Fix:** Teach `APIError` to decode problem+json: unmarshal into a problem struct first, map `Title`+`Detail`→`Message`, `Detail`→`Details`, `Code`→`Code` (keep legacy `message` back-compat). Stop gating `client.go:128` on `Message!=""` alone (also accept title/detail). Give `IsAuthError`/`IsConflict`/`IsValidationError` `StatusCode` fallbacks (401/403→auth, 409→conflict, 422→validation) mirroring `IsNotFound`. **CRITICAL:** rewrite the error tests to mock the *real* problem+json shape and assert `Message`/`Code` are populated from a `{title,detail}` body — the test gap that hid this.
- **Verifier rationale (confidence 92):** Confirmed at every cited location. Server: `Problem.Detail` is `json:"detail,omitempty"` (`problem.go:23`), `Code` is `json:"code,omitempty"` (`:30`); `WriteProblem` (`:42-53`) sets Type/Title/Status/Detail and never Code/message; all helpers (`:57-111`) route through it with no Code. Client: `IsAuthError`/`IsConflict`/`IsValidationError` (`errors.go:24-41`) gate only on `Code` (always `""`) with no StatusCode fallback; only `IsNotFound` (`:29-31`) checks `StatusCode==404`. Decode: `client.go:128` requires `Message!=""`; falls to `:132` `Message: string(respBody)`. Dead caller branches confirmed (`netgroup/delete.go:50-55`, `adapter/enable.go:67`). The audit's `cmd/dfsctl/cmd/` path was a minor typo for `cmd/dfsctl/commands/`; code matches.

### H1-RV — RE-VERIFIED round-1 H1: snapshot create `-o json/yaml` exits 0 on a `failed` snapshot
**`cmd/dfsctl/commands/share/snapshot/create.go:122-143`** *(round-1 finding — listed here only to confirm it still reproduces on current develop; counted in the round-1 tally, not as net-new)*

- **What:** The terminal switch on `format` handles JSON (`:123-124 return output.PrintJSON(...)`) and YAML (`:125-126 return output.PrintYAML(...)`) by returning the marshal result (nil on a successful write) **without inspecting `snap.State`**. Only the table/default branch (`:127-142`) checks `snap.State` and returns `fmt.Errorf("snapshot failed")`. No upstream guard short-circuits: `WaitForSnapshot` (`pkg/apiclient/snapshots.go:111-134`) returns `(snap, nil)` for *any* non-`creating` terminal state including `failed` (`:125-126 if snap.State != "creating" { return snap, nil }`), confirmed by `TestWaitForSnapshot_FailedState` (`snapshots_test.go:310-314`) asserting `require.NoError`.
- **Status:** Byte-identical to round-1; **PR-B not done.** Verified independently by two round-2 sub-audits (confidence 97). Fix and PR-B sequencing per round-1 §7 PR-B1 still applies. Not re-litigated here.

---

## 3. Triage downgrades / RESOLVED

No round-2 HIGH was refuted. Both proposed HIGHs (the RFC7807 decode drift and the re-verified round-1 snapshot H1) survived adversarial verification at HIGH. All other round-2 findings were proposed at MED/LOW and confirmed at that level — none was upgraded to HIGH (notably, the SMB-password-on-argv and credential-store-race findings were held at MED, consistent with how round-1 rated the analogous dfsctl `--password` argv exposure; they are local-disclosure / concurrency-integrity, not auth-bypass or data-loss).

---

## 4. MED findings

**error-surfacing — exit-0-on-failure class recurs beyond snapshot-create**
- **`store block audit-refcounts` exits 0 when it DETECTS refcount drift** (`cmd/dfsctl/commands/store/block/audit.go:38-90`; Delta check `:85-89`; JSON/YAML `:58-61`) — the INV-02 reconciliation audit (∑`FileBlock.RefCount` vs ∑`len(FileAttr.Blocks)`). When the server returns `res.Result.Delta != 0` — a real refcount-drift/corruption indicator (`Delta` is `int64` at `pkg/blockstore/engine/audit_state.go:75`; "non-zero" per the doc at `:72-74`; `TestAuditRefcounts_DetectsDelta` confirms) — the table branch *prints* "INV-02 violation" but `RunE` returns nil; JSON/YAML never inspect `Delta`. So `audit-refcounts <share> || alert` never alerts on detected corruption that "may block GC reclamation." Same class as round-1 H1, different command. **Fix:** after the format switch, `if res.Result.Delta != 0 { return fmt.Errorf("INV-02 violation: refcount delta=%d", res.Result.Delta) }` (mirror the correct pattern in `store/block/health.go:80-83`); add a `-o json` non-zero-Delta regression test. (confidence 85)
- **`store block gc` exits 0 when the GC sweep had delete errors** (`cmd/dfsctl/commands/store/block/gc.go:46-76`; `RunE` returns nil at `:70/72/74`; `ErrorCount` printed only at `:88/95-101`) — `Stats.ErrorCount` (`pkg/blockstore/engine/gc.go:140`, `int`, incremented per sweep-phase Delete/list error at `:472/527`) signals orphan objects not reclaimed (storage/cost leak, partial failure). The table branch prints it; the function returns nil in every format. **Fix:** after the format switch, `if res.Stats.ErrorCount > 0 { return fmt.Errorf("GC completed with %d sweep error(s)", res.Stats.ErrorCount) }`, still emitting the stats body first. (If GC errors are intentionally advisory, document the exit-0 contract in `docs/CLI.md` as migrate-status does — currently neither enforced nor documented.) Note `gc-status` (`gc_status.go`) legitimately returns nil because it is a read-only status report; `gc.go` is the operation. (confidence 70)

**credential-security — leaks at the external-mount-process seam + no concurrency story**
- **SMB mount password passed on argv to external `mount.cifs`/`net use`** (`cmd/dfsctl/commands/share/mount_unix.go:136,156` Linux; `mount_windows.go:126` Windows; macOS `smb://` URL `mount_unix.go:172-174,183`) — `resolveSMBPassword` (`mount.go:154-169`) returns cleartext, embedded directly in a child process's command line (`mount -t cifs -o ...,password=X`, `net use ... PASSWORD`). For the whole (potentially slow) mount it is visible to any local user via `ps`/`/proc/<pid>/cmdline`. Distinct from round-1's dfsctl-`--password` finding: this is the password re-exported into a *different* process's argv. The canonical tools provide argv-free mechanisms (`mount.cifs` `PASSWD` env / `credentials=<file>`). **Fix:** Linux — pass via `cmd.Env` `PASSWD=` or a 0600 `credentials=<path>` then unlink; Windows — `cmdkey`/`WNetAddConnection2` or document; macOS — interactive form or document the window. (confidence 90)
- **Linux/Windows SMB mount error output echoed unsanitized — password can leak into the error message** (`cmd/dfsctl/commands/share/mount_unix.go:157-159` Linux raw; `mount_windows.go:138-152` Windows raw; contrast macOS scrub `mount_unix.go:199-201`) — the macOS path explicitly scrubs (`strings.ReplaceAll(string(output), password, "****")`) before surfacing; Linux/Windows pass raw child output straight through. `mount.cifs`/`net use` can echo the supplied options (including `password=…`) on failure → cleartext to terminal/CI logs. The author recognized the risk (macOS scrub) but did not apply it elsewhere. **Fix:** extract a shared `sanitizeMountOutput(output, password)` and apply on all platforms (best combined with the prior finding so the password never reaches argv/output at all). (confidence 82)
- **Credential store read-modify-write has no file lock and a non-atomic write — concurrent `dfsctl` invocations silently lose token/context updates** (`internal/cli/credentials/store.go:133-157`; racing callers `cmdutil/util.go:109 UpdateTokens`, `login.go:127 SetContext`) — every mutation is a whole-file RMW with no flock, no `O_EXCL` temp+rename, no in-process mutex. Two concurrent processes each load the same baseline; last `save()` wins. Concretely reachable: `GetAuthenticatedClient`'s auto-refresh (`util.go:94-114`) calls `UpdateTokens` after a round-trip — two commands with an expired token both refresh and write, so one freshly-rotated refresh token is silently overwritten by the other's stale value, poisoning the next refresh cycle (forced re-login) on a rotating-token server. A `login` racing any command can likewise be clobbered; the non-atomic `os.WriteFile` also lets a concurrent reader observe a truncated `config.json` (corrupting *all* contexts+tokens). Round-1 noted the non-atomic-write MED but not the concurrent lost-update / refresh-poisoning interaction. **Fix:** make `save()` atomic+serialized — temp file (0600) in the same dir, fsync, `os.Rename`, re-`Chmod` 0600 (also closes round-1's perm-on-rewrite gap); take an advisory flock on the config dir around load→mutate→save, or re-load-and-merge immediately before save. (confidence 78)

**cli-cross-binary — status contract drift across the two front-ends**
- **Cross-binary status drift: incompatible JSON shapes + non-canonical `DFS_API_TOKEN` env** (`cmd/dfs/commands/status.go:68-78,56,165`) — `dfs status` JSON (`running`/`pid`/`message`) and `dfsctl status` JSON (`server`/status-string/`error`) share **no** common liveness field, so automation cannot be written once against the two interchangeable front-ends. Separately, `dfs status` reads a non-canonical `DFS_API_TOKEN` env while config uses the `DITTOFS_` prefix, and `dfsctl` auth has no env-token path at all. **Fix:** one canonical status JSON shape in `internal/cli/health`; one `DITTOFS_`-prefixed token env + flag across both binaries. (confidence 80) (Pairs naturally with round-1's status exit-code MED in PR-B2.)

---

## 5. LOW findings

**dfsctl-rest-contract**
- **apiclient advertises `Accept: application/json` but the server replies `application/problem+json` on error** (`pkg/apiclient/client.go:110` vs `problem.go:39/50`) — harmless today (the client ignores `Content-Type` on decode), but a latent contract mismatch: a future content-negotiating server could 406 the CLI. **Fix:** `Accept: application/json, application/problem+json` (or drop the over-specific header). Align when H1's decode fix lands. (confidence 70)
- **`docs/CLI.md` documents no error/exit-code contract for the problem+json shape** (`docs/CLI.md:~530`, only an incidental "not found / auth failure / network error" line) — no authoritative source of truth for the client/server error contract, which is part of why the H1 drift went unnoticed. **Fix:** after the decode fix, add a short "Error output" section documenting that the server returns problem+json `{title,detail,status,code}` and that dfsctl surfaces `detail`/`title` as the message and maps status→exit semantics. Pair with the H1 fix. (confidence 55)

**error-surfacing**
- **`dfs config validate` prints "Validation: OK" and exits 0 even when a warning says auth WILL FAIL** (`cmd/dfs/commands/config/validate.go:44-67`; "Validation: OK" at `:53`, return nil at `:67`; JWT warning at `:45`) — `runConfigValidate` always prints OK + exits 0 after collecting warnings, including "JWT secret not configured - API authentication will fail" and deprecated-key warnings. `MustLoad` succeeded so these are non-fatal by construction, but a `config validate && start-server` gate treats a config that cannot authenticate as valid. Milder than gc/audit (warnings are genuinely non-fatal at load time; plain output shows them on screen). **Fix:** "Validation: OK (with N warning(s))" when warnings exist, or a `--strict` flag returning non-zero on any warning so CI can gate; at minimum do not print bare "OK" when the JWT-secret warning is present. (confidence 55)

---

## 6. Verified-correct

**Error surfacing (the exit-0 class is NOT universal — these are the correct pattern)**
- `store block health` (`store/block/health.go:80-83`) and `store metadata health` (`store/metadata/health.go:68-71`) check `!resp.Healthy` **after** the format switch and return a non-nil error in **all** output formats — the reference pattern gc/audit/snapshot-create should follow.
- `dfsctl share snapshot restore` (`restore.go:80-97`) surfaces server failures correctly — `RestoreSnapshot` error returns non-nil; 412-not-durable special-cased (`:87`); restore is synchronous so there is no async `failed`-state to miss (unlike create's `WaitForSnapshot`).
- `dfs migrate` (`migrate.go:46-56`) returns non-nil on `store.New` migration error and on `ListUsers` verification error; `cpStore.Close` deferred.
- `dfsctl system drain-uploads` (`drain_uploads.go:30-55`) — transport error returns non-nil; success response has no failure-state field to miss.
- `store block gc-status` (`gc_status.go`) legitimately returns nil while displaying `ErrorCount`/`FirstErrors` — it is a read-only status report (it succeeded at reading `last-run.json`); `ErrNoGCRunYet` correctly produces non-zero. Distinct from `gc.go` (the operation).
- client evict/disconnect, sessions-destroy, sessions/client list, grace end, settings set, adapter enable/disable, idmap add — all surface server failures via the `doVia`/`APIError` path (`StatusCode>=400` → non-nil); mutation commands return the post-op resource with no separate failure-state field, so no exit-0 gap.
- All fallible subcommands use `RunE` (111 handlers); `main.go` prints to stderr + `os.Exit(1)` under `SilenceErrors`/`SilenceUsage` — single error sink, non-zero exit on any returned error (confirms the only gaps are handlers that swallow a result-status field and return nil — the gc/audit/snapshot class).

**Credential security at rest and on the wire (round-1 verified-correct items re-confirmed on current develop)**
- No token/password logging anywhere in `pkg/apiclient`: `do`/`doVia` (`client.go:95-141`) sets the `Authorization: Bearer` header but never logs it; `auth.go` Login/RefreshToken never print credentials; error wrapping uses `%w` on the transport error, not the body.
- `--verbose` is parsed (`util.go:138-141 IsVerbose()`) but **not** consumed by apiclient to dump requests/headers/bodies — no token-leaking verbose path (effectively dead → a bloat, not security, concern).
- TLS verification on by default: `apiclient.New` builds an `http.Client` with only a `Timeout` (`client.go:39-51`) — no `tls.Config`, no `InsecureSkipVerify`, no transport override, no `--insecure`/`--skip-verify` flag anywhere.
- Credential file model sound at rest: `config.json` created `0600` in a `0700` dir under `XDG_CONFIG_HOME`/`%APPDATA%` (`store.go:14-23,103-130,144-157`); no path traversal in `getConfigPath`; `ClearCurrentContext` (`store.go:261-272`) zeroes access/refresh/expiry and re-saves.
- Token-refresh poison guard correct: `GetAuthenticatedClient` refuses `UpdateTokens` when refreshed access **or** refresh token is empty (`util.go:104-106`); `login.go:98-107` has the equivalent guard.
- macOS SMB mount path correctly URL-encodes username/password into the `smb://` URL (`url.PathEscape`, `mount_unix.go:172-174`) and sanitizes the password out of failure output (`mount_unix.go:200`) — the localized mitigation is correct; the gap is only that Linux/Windows lack the equivalent (see §4).
- `resolveSMBPassword` honors a sensible precedence (flag > `DITTOFS_PASSWORD` env > masked interactive prompt, `mount.go:154-169`) — an argv-free path exists; the residual exposure is the downstream child-process argv (§4), not dfsctl's own flag handling.

---

## 7. Recommended PR-B shape

> Round-1's PR-B (B1–B5) is **not done** and still applies. Round-2 adds the following; sequence the net-new HIGH first.

**PR-B6 — RFC7807 error-contract fix (net-new HIGH, highest leverage).** Make `pkg/apiclient.APIError` decode problem+json (`title`/`detail`/`code`), drop the `Message!=""` gate at `client.go:128`, and add `StatusCode` fallbacks to `IsAuthError`/`IsConflict`/`IsValidationError`. **Rewrite the error tests** (`client_test.go:73`, `snapshots_test.go:132/166`, `users_test.go:64/123`, `auth_test.go:49/101`) to mock the real problem+json body and assert `Message`/`Code` are populated — the test gap that hid this. Same PR: align `Accept` header (LOW) and add the `docs/CLI.md` "Error output" section (LOW). Isolated, well-contained, mergeable first.

**PR-B7 — Exit-0-on-failure class sweep (folds into round-1 PR-B1).** Extend round-1's snapshot-create fix to `store block audit-refcounts` (`Delta!=0`) and `store block gc` (`ErrorCount>0`), and at minimum `config validate` (JWT-secret warning). Audit **every** result struct with an `Error`/`Delta`/state field for the same pattern (not just snapshot). Add `-o json` failure-path regression tests for each.

**PR-B8 — SMB-mount credential hardening (folds into round-1 PR-B3).** Keep the cleartext SMB password off the child process argv (Linux `PASSWD` env / `credentials=` file; Windows `cmdkey`; macOS document) and apply the macOS output-scrub on all platforms via a shared `sanitizeMountOutput` helper.

**PR-B9 — Credential-store atomicity + concurrency (folds into / supersedes round-1 PR-B3's temp+rename).** Atomic temp+rename+re-chmod **plus** advisory flock (or re-load-and-merge) around the load→mutate→save sequence to close the lost-update / refresh-token-poisoning race.

**PR-B10 — Cross-binary status canonicalization (folds into round-1 PR-B2).** One canonical status JSON shape in `internal/cli/health` consumed by both binaries; one `DITTOFS_`-prefixed token env+flag (retire `DFS_API_TOKEN`).

**Defer as issues:** none net-new beyond round-1's deferrals.

---

## 8. Coverage

**Audited (4 parallel round-2 sub-audits, missed-findings + integration lens):**
- **error-surfacing** — every `dfsctl`/`dfs` `RunE` handler (111) swept for the exit-0-on-failure class beyond round-1's single snapshot command; every result struct with an `Error`/`Delta`/state field inspected for table-only / never-inspected status handling.
- **dfsctl-rest-contract** — the `pkg/apiclient` ↔ server REST error contract end-to-end: `WriteProblem` problem+json shape vs `APIError` decode, the `Is*` classifier helpers, the `Accept`/`Content-Type` seam, and the test suite's mocked error shapes vs what the server actually emits.
- **credential-security** — depth pass on `cmd/dfsctl` + `internal/cli/credentials`: the external-mount-process argv/output seam (Linux/Windows/macOS), token/password logging, TLS default, file perms, refresh-poison guard, and the credential-store concurrency model.
- **cli-cross-binary** — re-verification of round-1 H1 on current develop, and the `dfs`-vs-`dfsctl` status JSON shape + auth-env divergence across the two interchangeable front-ends.

**Re-verified from round-1 (still reproduce, PR-B not done):** round-1 H1 (snapshot create `-o json/yaml` exit-0-on-`failed`) — confirmed verbatim at `create.go:122-143` by two independent sub-audits; the round-1 non-atomic-credential-write MED is subsumed and broadened by the round-2 concurrency finding.

**Not audited (out of CLI scope; cross-referenced only where the CLI touches them):**
- The REST API server, Runtime, and store internals behind the CLI (covered by area audits #1, #4, #5, #6) — only the `apiclient`↔`WriteProblem` contract surface and the `audit-refcounts`/`gc` result-struct semantics (`pkg/blockstore/engine/audit_state.go`, `gc.go`) were read, to ground the error-surfacing findings.
- End-to-end behavioral testing against a live server, and concurrent-`dfsctl` stress against a shared config dir (static + test-coverage analysis only) — the credential-store race is reasoned from code, not reproduced under load.
