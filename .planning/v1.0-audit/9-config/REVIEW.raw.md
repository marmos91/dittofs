I'll synthesize the REVIEW.md from the verified findings data. Note the verifier downgraded several HIGHs: the no-config-file env-drop (HIGH→MED), and both dead-config findings (HIGH→MED). I'll reflect the adjusted severities.

# Config (pkg/config/) — v1.0 Area Audit REVIEW

**Status:** PATCH-grade with one real HIGH (secret disclosure) and one real HIGH (env-precedence correctness)
**Date:** 2026-06-01
**Scope:** `pkg/config/` (~1.2K LOC): `config.go`, `defaults.go`, `init.go`, `blockstore.go`, `validation.go`, plus the snapshot/syncer/lock config sub-trees and the secret-bearing embedded types (`api.JWTConfig.Secret`, `store.PostgresConfig.Password`, `AdminConfig.PasswordHash`). Touches `cmd/dfs/commands/config/show.go` and `cmd/dfs/commands/start.go` for wiring/consumption analysis.
**Cross-check refs:** area-4 "invalid config silently produces broken-but-running server" HIGH class; less-is-more / delete-eagerly conventions; GORM v1.31.1 bool zero-value default-substitution bug class; `docs/CONFIGURATION.md` precedence claims.

---

## 1. Summary

4 parallel sub-audits. Every raw HIGH was independently adversarially verified; three were downgraded on blast-radius grounds (no auth bypass / no data loss / server runs correctly on defaults).

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| parsing-precedence | 1 | 1 | 3 | 0 |
| security | 1 | 1 | 0 | 0 |
| surface-bloat | 0 | 2 | 0 | 0 |
| correctness-edge | 0 | 3 | 1 | 0 |
| **Total (post-verification)** | **2** | **7** | **4** | **0** |

Raw sub-audit tallies summed to 5 HIGH; adversarial verification confirmed 2 HIGH and downgraded 3 to MED (see §3).

**Verdict: PATCH-grade, but ship the 2 HIGHs before v1.0.** Neither HIGH is an architectural integrity hole — both are localized: (1) `dfs config show` leaks live secrets to stdout/CI logs (real disclosure → token-forgery / DB-access risk), and (2) env-var overrides are silently dropped for any key absent from the config file, directly contradicting the documented "env = highest priority" precedence. Both are self-contained fixes. The remaining surface is dominated by **dead config** (two entire sub-trees parsed but wired to nothing) and **missing redaction/validation** — debt, not corruption.

**Architecture invariants hold.** No metadata/blockstore-routing, file-handle-opacity, per-share-blockstore, or WRITE-ordering invariants are touched by the config layer. The GORM bool zero-value clobber class does **not** recur here (verified). Insecure-by-default surface is otherwise sane: config files are written `0600`, Kerberos/auth is opt-in, the postgres DSN is never logged, and the JWT min-length gate is fail-closed at server construction.

---

## 2. HIGH findings

Ranked by blast radius.

### 2.1 Secret disclosure

**`dfs config show` prints the JWT secret, postgres password, and admin password hash with no redaction** — `cmd/dfs/commands/config/show.go:68-73`

- **What:** `runConfigShow` loads the real config via `config.MustLoad` and dumps the entire `*config.Config` to stdout via `output.PrintYAML` / `output.PrintJSON`. The secret-bearing fields serialize verbatim: `JWTConfig.Secret` is tagged `yaml:"secret"` (`pkg/controlplane/api/config.go:53`); `store.PostgresConfig.Password` is an exported field with no exclusion tag (`pkg/controlplane/store/gorm.go:47`), reachable via `Config.Database → store.Config.Postgres → Password`; `AdminConfig.PasswordHash` serializes as `password_hash,omitempty` (`pkg/config/config.go:313`). None of these types implement `MarshalYAML`/`MarshalJSON`/`String` redaction (grep-confirmed empty), and both encoders call plain `Encode`. So `dfs config show` (and `--output json`) echo the live HMAC signing key and DB credentials.
- **Why:** The JWT secret is the HMAC signing key for all control-plane auth tokens — disclosure allows forging admin tokens (full auth bypass). The postgres password grants direct DB access. `config show` is a routine, low-friction command whose output lands in terminals, CI logs, and issue pastes. `docs/CONFIGURATION.md:228` explicitly tells users the secret "should be kept confidential" — yet the tool prints it.
- **Fix:** Redact before serializing. Cleanest: add `MarshalYAML`/`MarshalJSON` to `JWTConfig` and `PostgresConfig` emitting a fixed sentinel (`"********"` when non-empty, `""` when empty), and redact `AdminConfig.PasswordHash`. Alternatively build a redacted shallow copy in `runConfigShow`. Add a test asserting the secret value never appears in show output.
- **Verifier:** Confirmed against actual code — no redact/`REDACTED` logic anywhere in the show path. The runtime `GetJWTSecret()` env-preference is irrelevant here: any `JWT.Secret` present in the file is printed verbatim, and the postgres password + admin hash are serialized unconditionally. HIGH justified.

### 2.2 Precedence correctness

**Env-var overrides are silently dropped for any key absent from the config file (contradicts documented precedence)** — `pkg/config/config.go:460` (`v.Unmarshal`); root cause `setupViper` 539-557; doc claim 36-40 + `docs/CONFIGURATION.md:1376-1390`

- **What:** `Load()` relies on viper `AutomaticEnv()` + `v.Unmarshal()`. This is the well-known viper gotcha: `AutomaticEnv` only feeds `Unmarshal` for keys that already exist in the loaded config file (or are explicitly `BindEnv`'d). Any `DITTOFS_*` env var whose corresponding key is **not** written in the config file is silently ignored on `Unmarshal`. Only the 8 keys explicitly bound at `config.go:550-557` (`database.postgres.*`, `controlplane.secret`, `controlplane.pprof`) survive when absent from the file. Empirically verified with a temp config that omitted the keys: `DITTOFS_CONTROLPLANE_PORT=9999` → stayed 8080; `DITTOFS_LOGGING_LEVEL=ERROR` → stayed INFO; `DITTOFS_KERBEROS_ENABLED=true` → stayed false; `DITTOFS_LOCK_MANDATORY_LOCKING=true` → stayed false. `TestLoad_EnvironmentVariables` (`config_test.go:236`) passes only because its config-file body happens to contain `logging.level` (251) and `controlplane.port` (257).
- **Why:** A security-relevant or operationally critical setting set **only** via env (the standard containerized/12-factor pattern with a minimal/templated config file) is silently dropped — no error, no warning. Concretely affects auth posture (`kerberos.enabled`), locking semantics (`mandatory_locking`), and log level. The operator believes the override took effect. The JWT secret is the one exception that is safe (separate `os.Getenv` path), which is why this is HIGH not CRITICAL.
- **Fix:** Stop relying on `AutomaticEnv`+`Unmarshal` alone. Robust options: (a) seed viper defaults for every struct field via reflected-`SetDefault` (so `AutomaticEnv` has a key to bind), (b) `BindEnv` every key by walking struct tags, guarded by a test that fails when a new field lacks a binding, or (c) decode the file into a map, overlay env, then `Unmarshal`. Add a regression test that loads a config file **omitting** a key and asserts the env override lands.
- **Verifier:** Confirmed by reading the code and grep (zero `SetDefault`/`BindPFlag`/`MergeConfigMap`/`RegisterAlias`/`v.Set` calls in `pkg/config`). The cited unbound keys (`lock.mandatory_locking` config.go:249, `kerberos.enabled` config.go:327) are real and neither file-defaulted nor bound. Docs match the claim (config.go:36-40 + CONFIGURATION.md:1380/1389). Kept HIGH — silent, warning-free, affects auth posture in the standard minimal-config pattern.

---

## 3. Triage downgrades / RESOLVED

No findings were refuted outright. Three raw HIGHs were downgraded to MED on blast-radius grounds:

**`Load()` returns defaults and ignores ALL env vars when no config file exists — HIGH → MED** (`pkg/config/config.go:454-456`)
The `if !configFileFound { return GetDefaultConfig(), nil }` short-circuit never consults viper/env; empirically `DITTOFS_CONTROLPLANE_PORT=9999` with no file → port 8080. **Bug is real** and contradicts `Load()`'s own godoc (config.go:430-433). Downgraded because the dangerous path is blocked: the server `start.go`/`migrate.go` go through `MustLoad` (config.go:484-503), which hard-errors (with `dittofs init` instructions) when no file exists, so the server cannot silently run on env-less defaults. Real exposure is limited to the read-only `dfs logs` diagnostic (`cmd/dfs/commands/logs.go:60`) and hypothetical external direct-`Load()` callers. A footgun, not operator-facing silent misconfiguration of a running server. (Fix folds naturally into the §2.2 PR.)

**`config.LockConfig` (cfg.Lock) is entirely dead config — HIGH → MED** (`pkg/config/config.go:226-257`)
The whole Lock sub-tree (`MaxLocksPerFile`/`MaxLocksPerClient`/`MaxTotalLocks`/`BlockingTimeout`/`GracePeriodDuration`/`MandatoryLocking`/`LeaseBreakTimeout`) is declared, parsed, and partially defaulted but never read by startup/runtime — the only reference is `applyLockDefaults` (defaults.go:25). The live limiter is a separate `pkg/metadata/lock.Config` (`DefaultConfig()` = 1000/10000/100000), and `LeaseBreakTimeout`/`GracePeriodDuration` are DB-backed adapter settings. **Confirmed accurate** via exhaustive grep. Downgraded because it causes no corruption/crash/auth-bypass — it is misleading dead config + bloat, not the "broken-but-running" data path. Operator-set lock limits are silently ignored (the footgun), but the server runs correctly on package defaults.

**`config.SyncerConfig` (cfg.Syncer) is dead config — HIGH → MED** (`pkg/config/config.go:87-132`)
`UploadConcurrency`/`ClaimTimeout`/`Tick` are parsed, defaulted, validated, and round-trip-tested (`syncer_test.go`) but never consumed. The engine syncer is built in `runtime/shares/service.go:544` via `buildSyncerConfigFromDefaults(syncerDefaults)` from a DB-backed per-share defaults object whose field set is **disjoint** from `SyncerConfig`. `engine.DefaultConfig()` hardcodes `UploadConcurrency:8`; there is no `Tick` field in `engine.SyncerConfig` at all. The doc comment config.go:71-73 ("apply globally to every share's `*engine.Store` syncer") is provably false. **Confirmed accurate.** Downgraded because concurrency is in fact bounded via the deduced `ParallelUploads` path, so the DoS-adjacent framing is overstated; misleading dead knob + bloat, server runs correctly on defaults.

---

## 4. MED findings

**Security**
- **JWT secret strength validated only at API server construction, never at config load** — `pkg/config/validation.go:21-43`. `config.Validate` runs struct-tag + per-sub-config validation but has no check on `JWTConfig.Secret` length; the only `>=32` gate lives in `api.NewServer` (`server.go:62-63`). `config.Load` succeeds for an empty or 8-char secret. The placeholder `REPLACE-ME-WITH-SECURE-SECRET-MIN-32-CHARS` (init.go:67-69) passes the length gate while being a publicly-known constant. Fail-closed for the running server (NewServer aborts), so MED. Fix: add a JWT-secret check to `config.Validate` when the control plane is active — reject empty/`<32`-char secrets and the known placeholder, after applying `GetJWTSecret` env precedence; keep the `NewServer` check as defense-in-depth.

**Surface-bloat / correctness-edge (the dead-config cluster, see §3 for full detail)**
- **`config.SyncerConfig` is dead** — `pkg/config/config.go:71-132`. Delete struct + wiring (`defaults.go:27`, `validation.go:30`, `syncer_test.go`); live path is DB-backed per-share defaults.
- **`config.LockConfig` is dead** — `pkg/config/config.go:223-257`. Delete field (config.go:61), struct, and `applyLockDefaults` (`defaults.go:25,96-105`); real config is `metadata/lock` + adapter grace at `start.go:213-216`.
- **`KerberosConfig` has no `Validate()`; `MaxContexts<=0` means unlimited GSS contexts (memory DoS) and keytab/principal misconfig fails late** — `pkg/config/config.go:324-361`. Kerberos is absent from the `validation.go` fan-out. `MaxContexts` flows to `gss.NewContextStore` where `0` = unlimited (`internal/adapter/nfs/rpc/gss/context.go:208`); `applyKerberosDefaults` sets 10000 only when `==0`, so a **negative** override yields an unbounded context store. When `Enabled=true` there is no load-time check that `KeytabPath`/`ServicePrincipal` are set or the keytab exists — failure surfaces as a `Warn` that silently disables GSS for NFS (`nlm.go:635`) or an SMB build error (`start.go:362`), i.e. a silent auth downgrade. The default protects the common case, hence MED. Fix: add `KerberosConfig.Validate()` to the fan-out — reject `MaxContexts<0` (treat `0` as "use default", not "unlimited"), reject negative `MaxClockSkew`/`ContextTTL`, and when `Enabled` require non-empty `KeytabPath`+`ServicePrincipal` with an `os.Stat` so misconfig fails fast.

---

## 5. LOW findings

**parsing-precedence**
- **Documented "CLI flags (highest priority)" precedence tier does not exist** — `pkg/config/config.go:36-40` (mirrored `docs/CONFIGURATION.md:1376-1382`). No `viper.BindPFlag`/flag-to-config binding anywhere; the only flags (`--config`, `--foreground`/`--pid-file`/`--log-file`) select the file path or daemon behavior. Per less-is-more, correct the doc to `env > file > defaults` rather than adding the flag layer.
- **`BindEnv("controlplane.secret")` targets a non-existent key** — `pkg/config/config.go:556`. The struct path is `controlplane.jwt.secret` (api/config.go:39,53), so the binding populates nothing; harmless only because `GetJWTSecret()` reads `os.Getenv` directly. Remove the dead binding (a future refactor trusting it would silently break secret loading).
- **Typo'd/unknown config keys are silently ignored (no strict-decode)** — `pkg/config/config.go:460` (`Unmarshal` without `ErrorUnused`). A misspelled top-level key (`loggin:`) is dropped and the section falls back to defaults, verified empirically. Partially intentional (the removed `claim_batch_size` key relies on unknown-keys-don't-error), so don't enable `ErrorUnused` globally; instead emit a startup WARN listing `viper.AllKeys()` not matched by the struct.

**correctness-edge**
- **`SyncerConfig.Validate` only checks `UploadConcurrency`; negative `ClaimTimeout`/`Tick` pass** — `pkg/config/config.go:127-132`. Mostly moot (dead config, and the engine re-guards `<=0` at `syncer.go:148/151`). If the struct is kept, add `ClaimTimeout`/`Tick >= 0` checks; otherwise delete per §3.

---

## 6. Verified-correct

Checked and found OK:

- **Malformed values error cleanly, no silent default fallback.** A bad duration (`shutdown_timeout: not-a-duration`) returns `failed to unmarshal config: ... invalid duration` via `durationDecodeHook` (config.go:655); same for `byteSizeDecodeHook`. (Verified empirically.)
- **JWT secret is NOT subject to the env-drop bug.** `api.GetJWTSecret` (`pkg/controlplane/api/config.go:90`) reads `os.Getenv(DITTOFS_CONTROLPLANE_SECRET)` directly and warns when env overrides a file value — env precedence is correct here.
- **JWT min-length `>=32` is enforced fail-closed.** `api.NewServer` (`server.go:62-63`) refuses to start with a shorter secret — no auth-bypass-via-weak-default.
- **Postgres connection env vars work even when absent from the file** because they are explicitly `BindEnv`'d (`config.go:550-555`).
- **Bool zero-value vs unset is handled soundly; the GORM `false→true` clobber class does not recur.** `ApplyDefaults` uses `if x == 0`/`if x == ""` only for fields whose zero value is genuinely "unset"; `Kerberos.Enabled` and `Lock.MandatoryLocking` keep their `false` zero with no default-true clobber (`defaults.go:114-116,247-249`). Explicit `mandatory_locking: false` / `compress: false` survive `Load`.
- **Env precedence works for keys present in the file** (`TestLoad_EnvironmentVariables` passes legitimately for `logging.level` and `controlplane.port`).
- **Config files written `0600`** in `SaveConfig` (config.go:531), `InitConfig` (init.go:48), and `InitConfigToPath` (init.go:197) — appropriate given embedded JWT secret + password hash.
- **`generateJWTSecret` uses `crypto/rand`** for a 256-bit hex secret with a clearly-flagged placeholder fallback (init.go:64-71) — not a silent weak-key path.
- **`ApplyDefaults` composition** (defaults.go:19-31) delegates uniformly and runs on both the file-loaded path (config.go:465) and inside `GetDefaultConfig` (defaults.go:171).
- **Postgres DSN (with password) is never logged.** `gorm.Open` failure is wrapped without echoing the DSN (`gorm.go:198`); no startup code logs the full `Config` struct or the secret/password (`start.go:100-101,243`).
- **Kerberos/auth insecure-by-default is opt-in.** `Kerberos.Enabled` defaults `false`; API JWT auth is mandatory/always-on (no disable knob). The admin bootstrap password at `start.go:115` is a one-time, intentionally-shown generated value with a "will not be shown again" notice — acceptable bootstrap UX, not a config-struct leak.
- **Wired numeric knobs are correctly range-checked:** `GC.GracePeriod`, `GC.DryRunSampleSize`, `Snapshot.RestoreHTTPTimeout`, `Blockstore.DedupLRUSize`. Blockstore local/remote/s3 combo validation lives in `runtime/blockstore_init.go` (out of this sub-area's two files) — no invalid-combo gap found within `config.go`/`blockstore.go`.

---

## 7. Recommended PR-B shape

Split into three focused fix PRs; defer the rest as issues.

**PR-B1 — Config secret redaction (HIGH §2.1).** Add `MarshalYAML`/`MarshalJSON` redaction to `JWTConfig` and `PostgresConfig` (sentinel `"********"`/`""`), redact `AdminConfig.PasswordHash` in `config show`. Test: assert the live secret value never appears in YAML or JSON show output. Smallest, highest-priority — ship first.

**PR-B2 — Env precedence correctness (HIGH §2.2 + folds in the §3 no-file MED + LOW dead-`controlplane.secret` bind + LOW CLI-flags doc).** Seed viper defaults from a reflected default `Config` (or BindEnv-every-key with a struct-tag-walk guard test) so `AutomaticEnv` binds reliably; run env resolution on the no-file branch instead of short-circuiting to `GetDefaultConfig()`; delete the dead `BindEnv("controlplane.secret")`; correct the precedence doc to `env > file > defaults`. Tests: load a config file **omitting** a key and assert the env override lands; load with **no** file + env and assert the override lands. Reconcile `docs/CONFIGURATION.md`.

**PR-B3 — Dead-config deletion + Kerberos validation (the §3/§4 dead-config cluster + Kerberos MED).** Delete `config.LockConfig` and `config.SyncerConfig` (+ their `defaults.go`/`validation.go` wiring and `syncer_test.go`) per delete-eagerly; the live knobs are DB-backed per-share/adapter settings. Add `KerberosConfig.Validate()` to the `validation.go` fan-out (reject `MaxContexts<0`, negative `MaxClockSkew`/`ContextTTL`; require + `os.Stat` keytab/principal when `Enabled`). This is the bulk of the surface-debt reduction.

**Defer as issues (LOW):** unknown-key startup WARN (no `ErrorUnused`); `SyncerConfig.Validate` numeric checks (mooted if PR-B3 deletes the struct — close on merge).

---

## 8. Coverage

**Audited:**
- Parsing/precedence end-to-end: `Load`/`MustLoad`, `setupViper`, `AutomaticEnv`/`BindEnv`, `Unmarshal`, decode hooks (duration, byte-size), file-found vs no-file branches. Empirically reproduced env-drop and no-file behaviors.
- Defaults: `ApplyDefaults` composition and every per-section defaulter; bool zero-value vs unset handling (GORM-clobber class).
- Validation: `config.Validate` fan-out; presence/absence of per-sub-config `Validate()`; numeric range checks on wired knobs.
- Security: secret-bearing types (`JWTConfig.Secret`, `PostgresConfig.Password`, `AdminConfig.PasswordHash`), redaction (absent), file perms (`0600`), DSN/secret logging, `config show` output path, JWT min-length gate, `crypto/rand` secret generation, Kerberos/auth opt-in defaults.
- Consumption/wiring: traced `cfg.Lock` and `cfg.Syncer` from parse to (non-)use across `cmd/`, `pkg/`, `internal/`; confirmed the live lock limiter (`metadata/lock`) and engine syncer (`runtime/shares/service.go`) sources.
- Surface bloat: dead structs `SyncerConfig`, `LockConfig`.
- Caller analysis: enumerated `config.Load`/`MustLoad` callers to bound the no-file env-drop blast radius.

**Not audited (out of scope / deferred):**
- Blockstore local/remote/s3 **combo** validation, which lives in `runtime/blockstore_init.go` (outside `pkg/config`) — flagged as not-in-this-sub-area; worth a follow-up pass.
- Full downstream behavior of the DB-backed per-share/adapter settings that replace the dead config trees (adapter-settings audit territory).
- `internal/cli/output` encoder internals beyond confirming they perform no redaction.
- Concurrency/reload safety of config (no live-reload path exists; not exercised).
