# Phase 25: CLI + REST API + Documentation (parallel with 24) - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-28
**Phase:** 25-cli-rest-api-documentation-parallel-with-24
**Areas discussed:** CLI UX (create/restore/delete/flags), REST shape (restore sync vs async, create 202+poll, error mapping, timeout knob), List/Show output, Code structure (cobra package, Runtime wrappers, DTO, tests/PR), Docs scope, Cross-cutting rename (sync_gate → verify), Auth scope, E2E test coverage

---

## CLI create UX

| Option | Description | Selected |
|--------|-------------|----------|
| Block by default, --no-wait | CLI calls CreateSnapshot + WaitForSnapshot; --no-wait returns snapID immediately | ✓ |
| Return snapID immediately, --wait | Default fire-and-forget; --wait blocks | |
| Block always, no flag | Always block; no fire-and-forget | |

**User's choice:** Block by default with `--no-wait` escape.
**Notes:** Matches operator expectation "the command finishes when the thing is done"; scripts opt out via flag.

---

## CLI restore UX

| Option | Description | Selected |
|--------|-------------|----------|
| Refuse on enabled + Y/N prompt + --yes | Pre-flight refuses; interactive prompt; --yes skips | ✓ |
| Auto-disable + auto-re-enable + --yes + --no-auto-lifecycle | CLI bookends with disable/enable | |
| Refuse on enabled + --yes only (no interactive prompt) | Script-friendly; no interactive Y/N | |

**User's choice:** Refuse on enabled + Y/N + `--yes`.
**Notes:** Preserves D-24-01 spirit (explicit operator intent at each step). Success message includes safety snap ID + cleanup hint.

---

## CLI delete UX

| Option | Description | Selected |
|--------|-------------|----------|
| Y/N prompt + --yes (same as restore) | Consistent UX | ✓ |
| No prompt, --yes is no-op | Bare delete | |
| Refuse pre-restore-* by default + --yes override | Guardrail on recovery primitive | |

**User's choice:** Y/N prompt + `--yes`.
**Notes:** Uniform UX over special-casing safety snaps.

---

## CLI flag naming

| Option | Description | Selected |
|--------|-------------|----------|
| --no-sync-gate / --retry-of=<id> / --allow-non-durable | Direct struct-field mirror | |
| --no-verify / --retry=<id> / --force | Shorter, colloquial | ✓ |
| --no-sync-gate / --retry-of=<id> / --force-non-durable | Mixed | |

**User's choice:** `--no-verify` / `--retry=<id>` / `--force`.
**Notes:** Operator vocabulary. `--no-verify` choice drove the cross-cutting rename of `sync_gate` → `verify` for vocabulary consistency.

---

## REST restore: sync vs async

| Option | Description | Selected |
|--------|-------------|----------|
| Sync 200 + long handler timeout (D-24-02 verbatim) | Block on Runtime.RestoreSnapshot; configurable timeout | ✓ |
| 202 + poll like create | Async; requires new restore-tracking DB table — violates D-24-02 | |
| Sync 200 + streaming progress (chunked JSON lines) | Stream progress events | |

**User's choice:** Sync 200 + handler timeout.
**Notes:** Honors Phase 24 D-24-02. Caller (CLI/apiclient) sets matching http timeout.

---

## REST create response shape

| Option | Description | Selected |
|--------|-------------|----------|
| 202 + Location + body {snapshot_id, share} | Typed body + Location header | ✓ |
| 202 + Location only (empty body) | Pure HTTP-spec form | |
| 201 + full snapshot record | Asymmetric with async orchestration | |

**User's choice:** 202 + Location + body.
**Notes:** Matches Phase 23 D-23-13 + best for typed client decoding.

---

## Error mapping for 12 typed sentinels

| Option | Description | Selected |
|--------|-------------|----------|
| Per-handler inline switch | Each handler has own errors.Is chain | |
| Single table in handlers/snapshot.go (mapSnapshotError) | One func, single source of truth | ✓ |
| Promote to problem.go | Wider blast radius | |

**User's choice:** Single `mapSnapshotError(w, err) bool` helper in handler file.
**Notes:** One edit when a new sentinel lands in v0.17.

---

## REST timeout knob

| Option | Description | Selected |
|--------|-------------|----------|
| 30m default, server-side YAML config | snapshot.restore_http_timeout: 30m | ✓ |
| No new knob — inherit ctx | apiclient/CLI set deadline | |
| Two knobs: restore + create | Symmetric but overkill (create is fast) | |

**User's choice:** YAML knob default 30m.
**Notes:** Operator visibility into long-running restore boundaries.

---

## Cross-cutting rename: sync_gate → verify (user-raised mid-discussion)

| Option | Description | Selected |
|--------|-------------|----------|
| verify / NoVerify / verify_concurrency | Align with --no-verify CLI flag | ✓ |
| durability_check / SkipDurabilityCheck / durability_check_concurrency | Maximally explicit but verbose | |
| Keep sync_gate (do not rename) | Single divergence between internal + operator names | |

**User's choice:** Rename to "verify" across Go code + YAML + docs.
**Notes:** User: "sync_gate is not very clear". Pre-1.0 + "no compat shims" allows hard rename without aliasing.

---

## Verify concurrency knob (user-raised mid-discussion)

| Option | Description | Selected |
|--------|-------------|----------|
| verify_parallel_requests: 16 | Self-documenting rename | |
| verify_workers: 16 | Short worker-pool model | |
| Drop the knob — hardcode 16 | YAGNI; nobody tunes it | ✓ |

**User's choice:** Drop the YAML knob entirely; hardcode 16 in callers.
**Notes:** User: "verify_concurrency is pretty confusing. What is the purpose of the flag?". Aligns with `feedback_less_is_more`. Re-addable later as non-breaking change.

---

## List columns

| Option | Description | Selected |
|--------|-------------|----------|
| ID(short) NAME STATE DURABLE CREATED SIZE | Most-useful at-a-glance | ✓ |
| ID NAME STATE DURABLE CREATED (no size) | Lighter row | |
| Full ID NAME STATE DURABLE CREATED SIZE RETRY_OF | Includes retry chain | |

**User's choice:** 6-column default with short ID + relative CREATED + manifest hash count as SIZE.

---

## List sort + filters

| Option | Description | Selected |
|--------|-------------|----------|
| Newest-first default, --state, --name-prefix | Cover the common ops | ✓ |
| Newest-first default, no filters | Pipe to grep/jq | |
| --state, --name-prefix, --sort, --limit | Full surface | |

**User's choice:** Newest-first + two AND-filters; no sort/limit flags.

---

## Show detail fields

| Option | Description | Selected |
|--------|-------------|----------|
| Full Snapshot + manifest stats + disk paths | Adds manifest count, dump bytes, paths, RetryOf, Error | ✓ |
| DB record only (no disk lookup) | Faster, less info | |
| Full + --verify action | Plus re-run verify; new endpoint | |

**User's choice:** Full detail including disk-derived fields.

---

## Docs: SNAPSHOTS.md scope

| Option | Description | Selected |
|--------|-------------|----------|
| Full operator guide (mirror BLOCKSTORE_MIGRATION.md depth) | 400-600 lines; 13 sections | ✓ |
| CLI-first, lean | ~150-200 lines; link to ARCHITECTURE for internals | |
| Reference-grade (CLI + full failure matrix + internals) | Maintenance heavy | |

**User's choice:** Full operator guide. Becomes canonical snapshot doc.

---

## Docs: ARCHITECTURE.md + CLI.md + README.md scope

| Option | Description | Selected |
|--------|-------------|----------|
| Minimal touches (surgical edits per doc) | Smallest correct change; heavy lifting in SNAPSHOTS.md | |
| Full sections (architecture rewrite + dedicated CLI section + README paragraph + example) | More duplication risk but richer | ✓ |

**User's choice:** Full sections in each doc.

---

## CLI cobra package layout

| Option | Description | Selected |
|--------|-------------|----------|
| Nested package cmd/dfsctl/commands/share/snapshot/ with one file per leaf | Mirror share/permission/ | ✓ |
| Flat: 5 files in share/ with snapshot_* prefix | No new package | |
| Single file snapshot.go with all 5 cmds | Compact but harder to navigate | |

**User's choice:** Nested package, one file per leaf.

---

## Runtime API surface

| Option | Description | Selected |
|--------|-------------|----------|
| Add Runtime.{GetSnapshot,ListSnapshots,DeleteSnapshot} wrappers | Handler never reaches into r.store; symmetric with existing Runtime methods | ✓ |
| Handler calls r.store for Get/List directly; only Delete wrapper | Asymmetric | |
| All snapshot ops in new snapshots.Service sub-service | Phase 23 D-23-14 already rejected this | |

**User's choice:** 3 Runtime wrappers; handler defines `SnapshotRuntime` interface for testability.

---

## REST DTO shape

| Option | Description | Selected |
|--------|-------------|----------|
| apiclient.Snapshot DTO (decoupled from models.Snapshot) | Hides GORM fields; matches BlockStoreStats pattern | ✓ |
| Expose models.Snapshot directly | One less type; risks leaking schema | |

**User's choice:** Separate apiclient.Snapshot DTO.

---

## Test strategy + PR shape

| Option | Description | Selected |
|--------|-------------|----------|
| 3 layers + single PR / 4 plans / 2 waves | Handler unit + apiclient stubserver + CLI unit + e2e; 4-plan PR | ✓ |
| Same layers, 4 separate PRs | More CI cycles | |
| Single mega-plan, single PR | Faster to ship; harder to review | |

**User's choice:** 4 plans in 2 waves, single PR.

---

## REST auth scope

| Option | Description | Selected |
|--------|-------------|----------|
| Admin-only (inherit existing /shares/{name} RequireAdmin) | Zero-config; restore is destructive enough | ✓ |
| Admin write, admin+operator read | Two role middlewares | |

**User's choice:** Admin-only via inheritance.

---

## E2E test coverage

| Option | Description | Selected |
|--------|-------------|----------|
| Two e2e: full HTTP↔runtime + dfsctl binary↔stubserver | Covers HTTP wiring + CLI output regressions | ✓ |
| One e2e: HTTP↔runtime only | Skip CLI binary | |
| No new e2e — unit tests only | Risk: handler→runtime wiring bugs caught manually | |

**User's choice:** Two e2e layers under `test/e2e/snapshot/`.

---

## Claude's Discretion

- Exact HTTP-code mapping per sentinel in `mapSnapshotError` (D-25-09 suggested table is provisional; planner finalizes against `problem.go` helpers).
- `apiclient.Snapshot.ManifestCount` / `DumpBytes` — on every list row (one stat each) vs on-show-only (default: on-show-only, planner-overridable).
- SIZE column unit — manifest hash count vs metadata dump bytes vs both.
- `Runtime.DeleteSnapshot` lock-acquisition path — direct `snapshotDeleteLock` call vs `SnapshotHoldProvider.AcquireDeleteLock` (same underlying mutex).
- Stubserver pattern reuse for apiclient tests (extend existing helper if reusable).
- Optional P25-02a split if reviewer load on P25-02 is too high.
- SNAPSHOTS.md exact section ordering + worked-transcript verbosity (operator-doc tone judgment).

## Deferred Ideas

- Restore-progress streaming (chunked JSON lines or SSE)
- `--verify` flag on `show` (re-run VerifyRemoteDurability on demand)
- Pagination on `list` (--limit, cursors)
- `--sort` flag on `list`
- Refuse-to-delete `pre-restore-*` guardrail
- Operator-role read access (split admin-only)
- Auto-cleanup of safety snaps on restore success
- Manifest stats on every list row (planner-discretion default flips to on-show-only)
- OpenAPI spec for snapshot endpoints
- Prometheus / OTel metrics surface
- Async restore (202+poll) — Phase 24 D-24-02 explicit rejection
- Cross-share restore (snapshot of A → B)
- Snapshot encryption / per-snapshot keys
