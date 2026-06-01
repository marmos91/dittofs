# Config (pkg/config/) — v1.0 Area Audit REVIEW (Round 2)

**Status:** PATCH-grade — 4 HIGH (2 round-1 RE-VERIFIED still-open + 2 NEW cross-boundary), all secret-disclosure or silent-misconfiguration, none an architectural integrity hole
**Date:** 2026-06-01
**Scope:** ROUND 2 — missed-findings + integration lens, building on the round-1 REVIEW (every round-1 finding treated as KNOWN and not re-reported except where re-verified). Emphasis on cross-area/cross-component contract seams, error/failure paths, concurrency, and re-verification of round-1 HIGHs against current `develop`. Surfaces examined beyond round-1's `pkg/config` + `cmd/dfs/commands/config` + `start.go`: the `/api/v1/store/*` config endpoints (`internal/controlplane/api/handlers`), `pkg/apiclient`, `cmd/dfsctl`, `pkg/controlplane/store/gorm.go`, `pkg/config/init.go`, and the `dfs config validate`/`config schema`/`config show` encoder paths.
**Cross-check refs:** round-1 §2.1 (config-show secret leak) + §2.2 (env-precedence drop) + §3 (dead Lock/Syncer) + §4 (Kerberos no-Validate); area-4 "negative number disables a safety limit" DoS class; cross-area #11 (operator UI / rendered ConfigMaps); less-is-more / delete-eagerly conventions; `docs/CONFIGURATION.md` postgres-key + precedence claims; viper `AutomaticEnv`+`Unmarshal` gotcha.

---

## 1. Summary

4 parallel round-2 sub-audits, all under a cross-boundary / failure-path / concurrency lens. Every raw HIGH was independently adversarially verified; none was refuted.

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| env-precedence-deep | 2 | 1 | 1 | 0 |
| secret-surfaces | 2 | 0 | 0 | 0 |
| validation-completeness | 0 | 2 | 2 | 0 |
| config-consumption | 0 | 2 | 2 | 0 |
| **Total (post-verification, de-duplicated)** | **4** | **4** | **5** | **0** |

The 4 HIGHs de-duplicate to **4 distinct issues**: two are round-1 HIGHs RE-VERIFIED as still-open on current `develop` (no fix shipped — PR-B not done; git log confirms the last config-area touch predates the round-1 REVIEW), and two are NEW, both living at the cross-area boundaries round-1 audited each area in isolation and could not see. The NEW HIGHs are the headline of this round:

1. **Store-config API leaks credentials verbatim** — the `/api/v1/store/*` per-store config JSON blob returns the S3 `secret_access_key` and postgres `password` unredacted in REST responses and renders them in `dfsctl` output. This is the same secret class as round-1 §2.1 but on the network + CLI client surfaces (HTTP/proxy logs, operator UI, rendered ConfigMaps) — a strictly larger blast radius.
2. **Documented postgres config keys + the DB-backend env switch silently do not take effect** at the file/env parse boundary: `ssl_root_cert`/`max_open_conns`/`max_idle_conns` never bind (no struct tags), and `DITTOFS_DATABASE_TYPE=postgres` set via env (with `type` omitted from the file) is dropped — booting on local SQLite while the operator believes they are on HA Postgres.

**Verdict: PATCH-grade, but ship the 4 HIGHs before v1.0.** None is an architectural integrity hole. The config layer touches no metadata/blockstore/auth *data* path, is loaded once and immutable (no reload race), and the daemon re-exec inherits env correctly. The HIGHs are all of the same two shapes round-1 identified — secret disclosure and silent misconfiguration — now extended to the integration seams. The MED/LOW surface is dominated by validate-vs-runtime contract drift and unvalidated safety-limit footguns (the area-4 "negative disables the limit" class).

**Architecture invariants hold.** No metadata/blockstore-routing, file-handle-opacity, per-share-blockstore, or WRITE-ordering invariant is touched by config. Verified-clean integration properties: no request/response-body logging middleware echoes plaintext create-store secrets; the CLI credential store is `0600`/`0700`; blockstore-probe errors embed only ctx-cancel errors, not credentials; config is immutable post-boot (the only setters are mutex-guarded no-ops); daemon background→foreground re-exec does not override `cmd.Env`.

---

## 2. HIGH findings

Ranked by blast radius. The two NEW cross-boundary HIGHs lead; the two RE-VERIFIED round-1 HIGHs follow.

### 2.1 Store-config API + dfsctl leak S3 secret and postgres password verbatim (NEW — cross-area)

**The per-store config JSON blob (carrying S3 `secret_access_key` / postgres `password`) is returned unredacted in GET/List/Create/Update API responses and rendered verbatim by `dfsctl`** — `internal/controlplane/api/handlers/block_stores.go:55,318-327` (`BlockStoreResponse.Config` / `blockStoreToResponse`); mirror `internal/controlplane/api/handlers/metadata_stores.go:50,287-295`

- **What:** `BlockStoreResponse` and `MetadataStoreResponse` both carry `Config string \`json:"config,omitempty"\`` populated directly from `models.*Config.Config` (the stored JSON blob), with no redaction. For a remote S3 block store that blob contains `secret_access_key` (consumed at `pkg/controlplane/runtime/blockstore_init.go:78`; required per `pkg/blockstore/remote/s3/store.go:98`); for a postgres metadata store it contains the DB `password` (`pkg/metadata/store/postgres/config.go:15`). No `MarshalYAML`/`MarshalJSON`/`String` redaction exists on any of these types — grep for `redact`/`REDACTED`/`maskSecret`/`****` across `pkg/`, `internal/`, `cmd/` returns nothing (only an unrelated mount-password sanitize and an unrelated admin-bootstrap print). The leak then reaches operators two ways: `cmd/dfsctl/commands/store/block/remote/list.go:38-42` prints `string(s.Config)` directly into the **default-output** table CONFIG column (the S3 secret key shown with no `--json` flag), and `dfsctl ... list -o json` serializes the full `[]apiclient.BlockStore` including `Config json.RawMessage` (`pkg/apiclient/stores.go:14,24`) verbatim — dumping the postgres password.
- **Why:** Same secret class as round-1 §2.1 (`config show`) but on the network API + CLI surfaces round-1 explicitly did not audit, so the blast radius is strictly larger: API responses land in HTTP access logs, reverse-proxy/CDN logs, browser history, and the dittofs-pro operator UI; rendered into a Kubernetes ConfigMap they persist in cluster state and `kubectl get` (cross-area #11). The S3 secret access key grants direct object-store read/write (data exfiltration/destruction); the postgres password grants full metadata-DB access. Admin-only routing reduces but does not remove exposure — admins routinely paste `dfsctl ... list` output into tickets/CI, and any read-only log sink downstream of the API now contains live credentials.
- **Fix:** Redact secret-bearing keys in the config blob before it leaves the process. Cleanest: a redaction step in `blockStoreToResponse`/`metadataStoreToResponse` that parses the JSON blob and replaces known secret keys (`secret_access_key`, `password`, and by convention `*secret*`/`*password*`/`*_key`) with a fixed sentinel (`"********"`) — never emit the raw stored blob on read paths; keep Create/Update accepting plaintext. Add a handler test asserting a stored secret value never appears in any response body, plus a `dfsctl` test asserting redacted table/JSON output. Fold into round-1 PR-B1 (it is the cross-area sibling).
- **Verifier:** All load-bearing claims confirmed at the cited lines. Minor non-defeating note: there is no `get` subcommand under `store/block/remote` or `store/metadata` (only add/edit/list/remove) — the full-blob dump occurs via `list -o json`, which the finding also cites correctly. The `RequireAdmin` routing claim could not be confirmed at the exact cited path, but the leak primitive is independent of routing. HIGH justified.

### 2.2 Documented postgres keys + the DB-backend env switch silently do not take effect (NEW — config↔store parse seam)

This is one bug class with two concrete, separately-shippable instances, both at the `config ↔ store.Config` parse boundary.

**(a) `PostgresConfig`/`SQLiteConfig`/`store.Config` carry NO `mapstructure`/`yaml` tags — documented multi-word keys (`ssl_root_cert`, `max_open_conns`, `max_idle_conns`) silently drop from file AND env** — `pkg/controlplane/store/gorm.go:42-52` (fields, zero tags), reached via `pkg/config/config.go:50` (`Database store.Config`); doc keys `docs/CONFIGURATION.md:184-186`

- **What:** These structs are uniquely untagged (every config struct in `pkg/config` carries explicit tags). `config.go:460` calls `v.Unmarshal` with only a `DecodeHook` — no custom `DecoderConfig`/`MatchName`/`TagName` override — so mapstructure uses its default case-insensitive matcher, which does **not** insert underscores. Empirically reproduced with the exact go.mod version (`mitchellh/mapstructure v1.5.0`): `max_open_conns: 99` / `ssl_root_cert: /etc/ca.pem` (the exact spellings in the docs) decode to zero values; only the undocumented no-underscore forms (`maxopenconns`, `sslrootcert`, `maxidleconns`) bind. These keys also have no `BindEnv` (`setupViper` binds only `host`/`port`/`database`/`user`/`password`/`sslmode` at `config.go:550-557`), so env cannot set them either.
- **Why:** Two real impacts. (a) Pool tuning (`max_open_conns`/`max_idle_conns`) is silently ignored → the hardcoded 25/5 (`gorm.go:110-114`, applied via `SetMaxOpenConns`/`SetMaxIdleConns` at `:207-208`) — a latent capacity/exhaustion footgun under load. (b) **Security:** `ssl_root_cert` is un-settable by any means, so `sslmode: verify-full` (which *does* bind) verifies against the system trust store instead of the operator's intended private-CA pin (`gorm.go:62-63` omits `sslrootcert` from the DSN when empty) — a silent TLS-posture downgrade. `config.Validate` never references these fields and emits no unknown-key warning. HIGH is justified by the silent TLS downgrade dimension; the pool-tuning silent-ignore is unconditional.
- **Fix:** Add `mapstructure:"..." yaml:"..."` tags to every field of `store.Config`/`PostgresConfig`/`SQLiteConfig`; add `BindEnv` for `sslrootcert`/`maxopenconns`/`maxidleconns`/`sslmode` (or fold into the round-1 PR-B2 reflective-BindEnv walk). Add a round-trip test asserting the docs' exact postgres keys land in the parsed struct.

**(b) `DITTOFS_DATABASE_TYPE=postgres` set via env (container pattern, `type` omitted from file) is silently dropped — server boots on SQLite instead of HA Postgres** — `pkg/config/config.go:454-462,545` (no `BindEnv` for `database.type`); consumed at `cmd/dfs/commands/start.go:104` `store.New(&cfg.Database)`

- **What:** A distinct, higher-blast-radius instance of round-1 §2.2 (`AutomaticEnv`-drop) that round-1 did not enumerate. With a config file that supplies postgres connection details but **omits** `database.type` (the standard 12-factor/container pattern: base-image config + per-deployment env), `DITTOFS_DATABASE_TYPE=postgres` is silently ignored because the key is absent from the file map and is not `BindEnv`'d. Empirically: `cfg.Database.Type` resolves to `"sqlite"` (the default from `store.Config.ApplyDefaults`, `gorm.go:78-80`) and `store.New` opens a local SQLite file with no warning. When `type:` *is* present in the file the env override works (the key then exists in the map) — which is why this slips past `TestLoad_EnvironmentVariables` (`config_test.go:236-277`, which places `database.type: sqlite` in the file and only exercises in-file keys).
- **Why:** Silent persistence-backend mis-selection — the operator believes control-plane metadata (users/groups/shares/permissions) is on HA Postgres; it is actually on single-node SQLite. No error, no warning, and `config.Validate` cannot catch it because it omits `cfg.Database.Validate()` from its fan-out (`validation.go:21-43`) and the resulting sqlite config validates fine. Risk of split-brain / data divergence across replicas and silent loss of the intended durability/HA guarantee. Higher blast radius than round-1's port/log-level/kerberos examples because it changes the persistence backend.
- **Fix:** `BindEnv("database.type")` (ideally every key via the round-1 PR-B2 reflective walk — fixes the whole class at once). Add `cfg.Database.Validate()` to the `config.Validate` fan-out. Regression test: file omitting `database.type` + `DITTOFS_DATABASE_TYPE=postgres` must yield `Type==postgres`.
- **Verifier:** Confirmed at every cited location; the code's own comment at `config.go:547-549` documents the exact viper limitation the finding relies on. Non-defeating note: no in-repo docker-compose currently sets `DITTOFS_DATABASE_TYPE`, so the container pattern is plausible rather than demonstrably in active use — tempers exploitability slightly, does not refute the bug.

### 2.3 RE-VERIFY: `dfs config show` still prints JWT secret / postgres password / admin hash unredacted (round-1 §2.1, NOT fixed)

**`runConfigShow` still serializes the entire `*config.Config` to stdout with no redaction** — `cmd/dfs/commands/config/show.go:68-73`

- **What:** Re-verified against current `develop`: `show.go:68-73` calls `output.PrintJSON`/`PrintYAML` (plain `json.NewEncoder`/`yaml.NewEncoder`, zero redaction) on the live config. `JWTConfig.Secret` is still `yaml:"secret"` (`pkg/controlplane/api/config.go:53`); `AdminConfig.PasswordHash` is `password_hash,omitempty` (`pkg/config/config.go:313`); `PostgresConfig.Password` is a plain exported string reachable via `Config.Database` (`pkg/controlplane/store/gorm.go:47`). Grep confirms zero `MarshalYAML`/`MarshalJSON`/`String` redactors on any of these types. `git log` on `pkg/config/`, `cmd/dfs/commands/config/`, and `pkg/controlplane/api/config.go` shows no fix since the 2026-06-01 round-1 REVIEW (latest touch is an unrelated blockstore rename, `8fb33660`). Reproduces unchanged.
- **Why:** Confirms round-1 §2.1 is NOT yet fixed by any shipped PR (PR-B not done) and must still ship before v1.0. Disclosure of the HMAC signing key permits admin-token forgery (auth bypass); `config show` is low-friction and its output lands in terminals/CI/issue pastes. On-disk `0600` perms do not mitigate stdout disclosure.
- **Fix:** Ship round-1 PR-B1 as written (`MarshalYAML`/`MarshalJSON` sentinel redaction on `JWTConfig`/`PostgresConfig`, redact `AdminConfig.PasswordHash`, test asserting the live secret never appears). **Note:** these typed-field marshalers do NOT cover the §2.1 store-blob leak (that path stores credentials as an opaque JSON string, not a typed field) — **both** fixes are needed.

### 2.4 RE-VERIFY: env-precedence drop still reproduces (round-1 §2.2, NOT fixed)

**`AutomaticEnv`+`Unmarshal` still silently drops env overrides for any key absent from the file** — `pkg/config/config.go:545-557` (`AutomaticEnv` + exactly 8 `BindEnv` keys), `config.go:460` (`Unmarshal`)

- **What:** Re-verified empirically on current `develop`: `CONTROLPLANE_PORT`/`LOGGING_LEVEL`/`KERBEROS_ENABLED` all dropped when absent from the file; `CONTROLPLANE_PPROF` works only because it is one of the 8 explicitly `BindEnv`'d keys. Zero `SetDefault`/`MergeConfigMap`/struct-tag BindEnv-walk exists in `pkg/config` (grep-confirmed). The instances in §2.2(b) and §4.2(b) below are concrete high-blast-radius members of this same class that round-1 did not enumerate.
- **Why / Fix:** Per round-1 §2.2 — the documented "env = highest priority" precedence is contradicted silently. The fix (round-1 PR-B2: seed viper defaults from a reflected default `Config`, or BindEnv every key via a struct-tag walk guarded by a test) resolves §2.2(b) and §2.4 together. Regression test: load a file omitting a key, set the env override, assert it lands.

---

## 3. Triage downgrades / RESOLVED

No round-2 HIGH was refuted. All 4 raw HIGHs were adversarially verified and confirmed at HIGH (2 NEW, 2 RE-VERIFIED round-1). No round-2 finding was downgraded.

For completeness, the three round-1 HIGH→MED downgrades (no-file env short-circuit; dead `LockConfig`; dead `SyncerConfig`) were **re-confirmed still-accurate** on current `develop` (see §6) and are not re-litigated here.

---

## 4. MED findings

**validation-completeness / env-precedence-deep — validate-vs-runtime contract drift**

- **4.1 `config.Validate` omits `cfg.Database.Validate()` from its fan-out** — `pkg/config/validation.go:21-43`; `store.Config.Validate` at `pkg/controlplane/store/gorm.go:120-140,160`. `Validate()` fans out to Blockstore/Syncer/GC/Snapshot but never to Database, so `dfs config validate` (`cmd/dfs/commands/config/validate.go:32,53`) prints "Validation: OK" for `database.type: mysql` or a postgres block missing `host`/`database`/`user`; the failure surfaces only at `dfs start`/`migrate` inside `store.New`. Same class as round-1's missing-`Kerberos.Validate` MED, for Database — and the load-time guard that §2.2(b)'s env-dropped `DATABASE_TYPE` scenario needs. Cross-boundary contract drift (`config validate` is documented at `docs/CLI.md:209` as checking "invalid values"). Fix: add `if err := cfg.Database.Validate(); err != nil { return err }` to the fan-out (after `ApplyDefaults` populates the sqlite default path) + a `validation_test.go` case; keep `store.New`'s check as defense-in-depth.

- **4.2 postgres `max_open_conns`/`max_idle_conns` unvalidated — negative value disables the connection-count safety limit (unlimited connections)** — `pkg/controlplane/store/gorm.go:207-208` (`SetMaxOpenConns`/`SetMaxIdleConns`), fields `:50-51`, defaulter `:110-115` guards only `==0`. A negative override (`max_open_conns: -1`, or via env once §2.2(a) is fixed) is neither defaulted nor validated, and per `database/sql` semantics `SetMaxOpenConns(n<=0)` means **unlimited**. The area-4 DoS class verbatim: a fat-fingered/templated negative silently removes the open-connection cap → control-plane DB connection count grows unbounded → connection exhaustion against Postgres (which has its own `max_connections` ceiling) → control-plane degradation/wedge. Silent: no warning, server starts normally. (`MaxIdleConns<0` is less severe — disables the idle pool, a perf regression.) Fix: add `MaxOpenConns`/`MaxIdleConns >= 0` checks to `store.Config.Validate` (treat `0` as "use default", reject negative), or clamp in `ApplyDefaults`.

**config-consumption — encoder round-trip / operator-config seams**

- **4.3 `config show --output json` and `config schema` emit PascalCase keys + ns-int durations that don't round-trip through `Load`** — `cmd/dfs/commands/config/show.go:70`, `cmd/dfs/commands/config/schema.go:38-48`, `pkg/config/config.go:41-85`. The `Config` struct and all embedded types carry only `mapstructure:`/`yaml:` tags — zero `json:` tags. `config show --output json` emits `{"Logging":{"Level":"INFO"},"ShutdownTimeout":30000000000,...}` (PascalCase keys, durations as raw nanoseconds), which `Load` (expecting lowercase `logging`/`shutdown_timeout`) cannot re-parse. Worse, `config schema` (advertised for "IDE autocompletion" / "config file validation") runs `jsonschema.Reflect`, which also reads json tags: verified top-level keys are PascalCase (`Logging`,`ShutdownTimeout`,`Database`,…) — every key mismatches the real YAML namespace, so a validator/IDE using this schema rejects every valid `config.yaml`. The schema command's entire stated purpose is broken. Fix: add `json:` tags mirroring the yaml tags (or set the jsonschema reflector `FieldNameTag: "yaml"` and have `PrintJSON` use the yaml-keyed view; marshal durations as strings) + a test asserting `config schema` top-level keys equal the lowercase yaml keys and that JSON show output re-parses through `Load`.

- **4.4 `dfs init` generated config omits kerberos/gc/lock/snapshot/syncer/blockstore/postgres sections — makes the env-drop HIGH bite the DEFAULT install** — `pkg/config/init.go:74-159` (`generateYAMLWithComments`) + `config.go:545-557`. The generated template emits only logging/shutdown_timeout/database(sqlite)/controlplane/admin. Combined with the §2.4 env-drop root cause (only 8 keys bound), on a standard `dfs init` install, env overrides for `kerberos.enabled`, `gc.grace_period`, `lock.*`, `snapshot.*` etc. are all silently dropped — the keys are neither in the generated file nor bound. Round-1 framed §2.2 as affecting "minimal/templated" configs; the cross-area reality is the out-of-the-box install, for exactly the sections an operator is most likely to tune via env in a container (auth posture, GC). Fix: folds into round-1 PR-B2; independently, the template should at least include commented-out kerberos/gc/snapshot sections so the keys exist for `AutomaticEnv` to bind. Regression test: generate default config, load it, set a kerberos/gc env override, assert it lands.

---

## 5. LOW findings

**validation-completeness — Kerberos knobs (opt-in, hence LOW)**

- **5.1 Negative `kerberos.context_ttl` / `max_clock_skew` slip past the `==0`-only defaulter; negative TTL expires every GSS context on each 5m cleanup sweep** — `pkg/config/defaults.go:123-130`; consumed at `internal/adapter/nfs/rpc/gss/context.go:291` (cleanup, `cleanupInterval` 5m at `:171`) and `pkg/auth/kerberos/provider.go:78` (skew). `applyKerberosDefaults` substitutes only when `==0`; a negative `context_ttl` makes every live context satisfy the expiry predicate → deleted each sweep → every Kerberos NFS client re-establishes its RPCSEC_GSS context ~every cleanup interval (auth-availability churn); a negative `max_clock_skew` rejects valid tickets. Round-1 noted the missing `Kerberos.Validate` but framed the impact only as `MaxContexts<0`; the `context_ttl` runtime effect is the new corollary. Fix: fold into the recommended `KerberosConfig.Validate()` — reject negative `MaxClockSkew`/`ContextTTL`/`MaxContexts` (treat `0` as "use default").

- **5.2 `kerberos.identity_mapping.strategy` is neither validated nor consumed — non-`static` value silently degrades to static mapping** — `pkg/config/config.go:373`; defaulted to `static` at `defaults.go:138-139`; `BuildStaticMapper` (`config.go:403`, sole caller `pkg/adapter/nfs/nlm.go:641`) ignores `Strategy` and always builds a `StaticMapper`. So `strategy: ldap` (or any typo) is silently accepted and maps to static (DefaultUID/GID = nobody/nogroup for unlisted principals) — a silent authorization-mapping misconfig on an auth-relevant knob. Low blast radius (Kerberos opt-in; static is the documented-only strategy). Fix: validate `Strategy` against supported values in `KerberosConfig.Validate()`.

**config-consumption / env-precedence-deep — dead/misleading wiring**

- **5.3 `SetSnapshotDefaults`/`SnapshotDefaults{}` is empty-struct no-op wiring (bloat)** — `cmd/dfs/commands/start.go:193` + `pkg/controlplane/runtime/runtime.go:744-752`. `SnapshotDefaults` is a zero-field struct; `start.go:193` takes `r.mu.Lock()` to store it into `r.snapshotCfg` — a pure no-op. The only real snapshot knob (`Snapshot.RestoreHTTPTimeout`) flows via a separate path (`api.NewServer`, `start.go:238`). Misleading speculative surface; per delete-eagerly, remove `SnapshotDefaults`/`SetSnapshotDefaults`/`r.snapshotCfg`/the `start.go:193` call, keep the `RestoreHTTPTimeout` path.

- **5.4 `Validate()` fan-out validates the DEAD `Syncer` config but not the live-consumed `Kerberos` config** — `pkg/config/validation.go:30`. `cfg.Syncer.Validate()` is called though `cfg.Syncer` is dead (round-1 §3, re-confirmed), while `cfg.Kerberos` — every field of which IS consumed (`nlm.go:649-650`, `provider.go:78`) — has no `Validate()` and is absent from the fan-out. Validation effort is spent on a knob that does nothing while the live auth-relevant knob is unguarded. Fix: add `KerberosConfig.Validate()` to the fan-out; remove `cfg.Syncer.Validate()` when the dead struct is deleted (round-1 PR-B3).

- **5.5 Documented env override `DITTOFS_LOCK_LEASE_BREAK_TIMEOUT=5s` is stale/non-functional** — `pkg/config/config.go:255` (godoc), mirrored `defaults.go:101`. The override is doubly dead: `lock.lease_break_timeout` is unbound (env-dropped: env=5s → parsed 35s), and the whole `config.LockConfig` is dead (round-1 §3) — the live lease-break-timeout is a DB-backed per-adapter setting (`internal/controlplane/api/handlers/adapter_settings.go`); `start.go` reads `nfsSettings`, not `cfg.Lock`. The documentation-correctness corollary at the env boundary that round-1 did not call out. Fix: delete the stale instruction with `config.LockConfig` (round-1 PR-B3), or redirect to the adapter-settings path.

---

## 6. Verified-correct

Checked at the round-2 integration / failure-path / concurrency lens and found OK:

- **No HTTP request/response-body logging middleware exists** on the API (`pkg/controlplane/api` + `internal/controlplane/api`: no `httputil.Dump`/`RequestLogger`/body logging) — the plaintext secret in a Create-store request body is not echoed to logs.
- **CLI credential store writes `0600` files / `0700` dir** — `internal/cli/credentials/store.go:20-22,147,156` — appropriate for the stored bearer token.
- **`blockstoreprobe` error messages do not leak credentials** — `pkg/controlplane/runtime/blockstoreprobe/probe.go:94,133` embed only ctx-cancel `err.Error()`, not access/secret keys.
- **No GET `/api/v1/config` or system endpoint dumps the full server `*config.Config`** — `system.go` has no config-dump handler; the only full-config dump surface remains the local `dfs config show` (round-1 §2.1 / §2.3 here).
- **`dfsctl store metadata list` table omits the Config column** (only NAME, TYPE) — `cmd/dfsctl/commands/store/metadata/list.go:31,38` — so the postgres password is not in the default table (it still leaks via `-o json`, captured in §2.1).
- **No secret-redaction helper exists anywhere** — grep for `redact`/`REDACTED`/`****`/`maskSecret` across `pkg/`/`internal/`/`cmd/` returns only unrelated path-sanitize and bitmask hits — the absence is systemic, confirming both leak HIGHs are not missed-local-helper false positives.
- **All Kerberos fields ARE consumed (not dead):** `MaxContexts`+`ContextTTL` → `gss.NewGSSProcessor` (`nlm.go:649-650`, logged `:664-665`); `MaxClockSkew` → provider (`provider.go:78`) + service (`internal/auth/kerberos/service.go:115`); `Krb5Conf` → `provider.go:62`; `IdentityMapping` → `StaticMapper` (`nlm.go:641`).
- **LogRotation fully wired** — `cfg.Logging.Rotation.{MaxSize,MaxBackups,MaxAge,Compress}` → `config.InitLogger` (`config.go:769-772`) → `lumberjack.Logger` (`logger.go:143-149`); the init template (`init.go:92-98`) writes all four keys so they round-trip.
- **GC + Snapshot.RestoreHTTPTimeout + Pprof all wired** — GC via `rt.SetGCDefaults` → `blockgc.go:188-192`; `RestoreHTTPTimeout` → `api.NewServer` (`start.go:238`); `Pprof` → `NewRouter` (`server.go:79,82`).
- **Daemon re-exec inherits env correctly** — `daemon_unix.go:39-90` does NOT set `cmd.Env`, so the foreground child inherits `DITTOFS_*` across the background→foreground re-exec (no daemon-specific env drop).
- **Config is loaded once at boot and immutable** — single `MustLoad`, no live-reload path, no config-struct reload race; the only mutating setters (`SetGCDefaults`/`SetSnapshotDefaults`) are mutex-guarded in `runtime.go`.
- **`config validate` and `config show` share the same `MustLoad → Load → ApplyDefaults → Validate` path** (`config.go:465-475`) — malformed values error cleanly rather than silently falling back, consistent across commands.
- **round-1 dead-config re-confirmed** — `LockConfig` (only `applyLockDefaults` references it) and `SyncerConfig` (engine syncer built from DB-backed per-share `SyncerDefaults` at `start.go:178-182`) are STILL dead on current `develop` (round-1 §3 holds).

---

## 7. Recommended PR-B shape

Round-2 extends round-1's PR-B1/B2/B3, adding one new redaction surface and one new parse-binding fix. Suggested split:

**PR-B1 (extend round-1) — All secret redaction, config + API + CLI (HIGH §2.1 + §2.3).** Add `MarshalYAML`/`MarshalJSON` sentinel redaction to `JWTConfig`/`PostgresConfig` and redact `AdminConfig.PasswordHash` in `config show` (round-1 PR-B1 as written), AND add a blob-redaction step in `blockStoreToResponse`/`metadataStoreToResponse` that replaces `secret_access_key`/`password`/`*secret*`/`*password*`/`*_key` with `"********"` on read paths (write paths still accept plaintext). Tests: live secret never appears in `config show` output, in any GET/List/Create/Update store-config response body, or in `dfsctl ... list` table/JSON. These are two independent code paths (typed fields vs opaque JSON blob) — both needed. Highest priority.

**PR-B2 (extend round-1) — Env precedence + postgres key binding (HIGH §2.2 + §2.4, folds MED §4.4).** Seed viper defaults from a reflected default `Config` (or BindEnv every key via a struct-tag walk guarded by a test) so `AutomaticEnv` binds reliably — this fixes §2.4, §2.2(b) (`database.type`), and §4.4 (default-install drop) at once. Separately, add `mapstructure:`/`yaml:` tags to `store.Config`/`PostgresConfig`/`SQLiteConfig` and `BindEnv` `sslrootcert`/`maxopenconns`/`maxidleconns`/`sslmode` to fix §2.2(a). Tests: file omitting a key + env override lands; docs' exact postgres keys parse into the struct; file omitting `database.type` + `DITTOFS_DATABASE_TYPE=postgres` → `Type==postgres`.

**PR-B3 — Validation completeness (MED §4.1, §4.2; LOW §5.1, §5.2, §5.4) + dead-config deletion (round-1 §3 + LOW §5.3, §5.5).** Add `cfg.Database.Validate()` to the `config.Validate` fan-out (§4.1); add `MaxOpenConns`/`MaxIdleConns >= 0` to `store.Config.Validate` (§4.2); add `KerberosConfig.Validate()` (round-1 + reject negative `ContextTTL`/`MaxClockSkew`/`MaxContexts`, validate `Strategy` — §5.1/§5.2/§5.4) and remove `cfg.Syncer.Validate()`; delete dead `LockConfig`/`SyncerConfig` + the stale `LEASE_BREAK_TIMEOUT` godoc (§5.5) + `SnapshotDefaults` no-op wiring (§5.3). Tests: unsupported `database.type` and postgres-missing-host both fail `config.Validate`; negative pool size rejected.

**Defer as issues (MED/LOW):** `config show --output json` / `config schema` json-tag round-trip + schema key-namespace fix (§4.3 — non-security DX bug, but the `config schema` command is wholly broken, so this warrants a near-term issue rather than indefinite deferral).

---

## 8. Coverage

**Audited (round-2 missed-findings + integration lens):**
- **Cross-area secret surfaces:** the `/api/v1/store/*` block + metadata config endpoints (`internal/controlplane/api/handlers`), `pkg/apiclient/stores.go`, all `cmd/dfsctl/commands/store/{block/remote,metadata}` list/output paths; the stored-blob secret keys in `runtime/blockstore_init.go`, `blockstore/remote/s3`, `metadata/store/postgres`; request-body logging middleware (absent); CLI credential-store perms; probe-error credential leakage.
- **Config↔store parse boundary:** key-by-key enumeration of `store.Config`/`PostgresConfig`/`SQLiteConfig` tag bindings vs `docs/CONFIGURATION.md`; empirical mapstructure-v1.5.0 decode reproduction; `database.type` env-drop; DSN/TLS construction in `gorm.go`.
- **Validate-vs-runtime contract:** `config.Validate` fan-out vs `store.New`/`store.Config.Validate`; `dfs config validate` command behavior; safety-limit handling (`SetMaxOpenConns` negative→unlimited); Kerberos `context_ttl`/`max_clock_skew`/`strategy` defaulting and consumption.
- **Encoder round-trip / operator-config seams:** `config show --output json`, `config schema` (`jsonschema.Reflect`), `dfs init` generated template completeness.
- **RE-VERIFICATION of round-1 HIGHs** on current `develop` (git-log + grep + empirical): §2.1 secret leak and §2.2 env-drop both still reproduce; round-1 dead-config (Lock/Syncer) re-confirmed.
- **Failure/concurrency paths:** config immutability / reload race (none); daemon re-exec env inheritance; mutex-guarded runtime setters; full knob-to-consumer tracing (Kerberos/LogRotation/GC/Snapshot/Pprof all wired).

**Not audited (out of scope / deferred):**
- Full dittofs-pro operator-UI and rendered-ConfigMap behavior downstream of the store-config API (cross-area #11 territory — the API-side leak is confirmed; the UI consumption is assumed).
- Downstream behavior of the DB-backed per-share/adapter settings that replace the dead config trees (adapter-settings audit territory).
- `internal/cli/output` encoder internals beyond confirming no redaction and the json-tag mismatch.
- Blockstore local/remote/s3 *combo* validation in `runtime/blockstore_init.go` (round-1 carried this forward; still outside `pkg/config`).
- Live exploitation of `DITTOFS_DATABASE_TYPE` via an actual container deployment (mechanism confirmed in-code; no in-repo compose currently exercises it).
