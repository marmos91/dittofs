# Area 11 — Kubernetes Operator (`k8s/dittofs-operator/`) — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Branch**: Operator audit run on `v1.0/blockstore-perf-b1` tree (= `develop` + B1/B2/B3 perf). PR-A to be opened off `develop`.
**Date**: 2026-06-01.
**Scope**: `k8s/dittofs-operator/` — ~5.7K Go LOC. controller-runtime + kubebuilder v4. CRD `DittoServer` (`api/v1alpha1/`: types + webhook + builder + helpers + `zz_generated.deepcopy`). Reconcilers (`internal/controller/`): `dittoserver_controller.go`, `adapter_reconciler.go`, `auth_reconciler.go`, `service_reconciler.go`, `networkpolicy_reconciler.go`, `dittofs_client.go` (REST client to `dfs`) + `config/`.
**Cross-check refs**: controller-runtime + kubebuilder canonical patterns; `k8s.io/apimachinery/pkg/api/meta`; RFC 7807 (problem+json); DittoFS server `internal/controlplane/api/handlers/` + `pkg/config` + `pkg/apiclient`.

**Method**: 6 parallel read-only sub-audits — reconcilers, api-types, security, client-integration, config-mgmt, simplicity-conventions. Every HIGH was independently adversarially re-verified against the cited source; refuted HIGHs were downgraded (none this run — both HIGHs survived verification).

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| reconcilers | 0 | 2 | 3 |
| api-types | 0 | 1 | 3 |
| security | 1 | 1 | 1 |
| client-integration | 1 | 1 | 0 |
| config-mgmt | 0 | 3 | 1 |
| simplicity-conventions | 0 | 0 | 9 |
| **Total** | **2** | **8** | **17** |

**Architecture invariants: clean.** The operator is a competently-built kubebuilder v4 controller. Child resources consistently use `SetControllerReference` for owner-reference GC and `controllerutil.CreateOrUpdate` (or Get-then-Create/Update with `IsAlreadyExists`/`IsNotFound` handling) — no blind-Create hot-loop. Watch wiring (`For`/`Owns`) is idiomatic; `Owns(PerconaPGCluster)` is gated on a RESTMapping existence check (graceful degradation). The finalizer has a 60s force-removal escape hatch (no deadlock). Secrets never land in the ConfigMap, CR status (only a SHA256 `ConfigHash`), events, or logs — all credentials inject via `SecretKeyRef` env vars; JWT/admin/operator passwords use `crypto/rand`. RBAC is least-privilege (the only `*` is scoped to the `dittoservers` CRD in the standard aggregated-admin convenience role).

**Verdict: NEEDS-FIX before tag.** Both HIGHs are *liveness-blocking* same-repo drift the operator's own tests actively mask:

1. **client-integration HIGH** — the operator parses errors as `{code,message,details}` but the `dfs` server emits RFC 7807 `{type,title,status,detail}` and never populates `code` or `message`. A typed `*DittoFSAPIError` is effectively never returned, so `IsConflict`/`IsNotFound`/`IsAuthError` are all dead and the operator-service-account conflict-tolerance path is unreachable — **reconcile never converges after the operator user first exists.**
2. **security HIGH** — the Helm chart's manager ClusterRole is missing the `networkpolicies` grant that the code treats as security-critical. Chart-deployed operators (the documented install path) cannot create NetworkPolicies, so every reconcile errors at the NP step, the network-isolation control never exists, and **the CR never reaches Ready.**

Both fail silently in CI: HIGH-1 is hidden by fabricated `{code,message}` test mocks the real server cannot emit; HIGH-2 is RBAC drift between `config/rbac` (source of truth) and `chart/`. This is the exact "green CI ≠ correct" class. Everything else is PATCH-grade: schema-drift traps (stale CRD `metrics` field, dead `cache:`/`admin.username` config keys), a status one-cycle lag, and repo bloat (a 55 MB committed binary).

**Theme**: the operator's structural backbone is sound; the holes cluster in **cross-boundary contracts with no shared source of truth and no round-trip/contract test** — operator↔server error shape, operator↔server config schema, `config/rbac`↔`chart/` RBAC. Each drifted because the only test coverage mocks the operator's *expectation* rather than the server's *actual* output.

---

## 2. HIGH findings (verified, ranked by blast radius)

### Cross-repo contract drift (liveness-blocking)

- **H1 — Silent error-shape API drift: operator parses `{code,message,details}` but `dfs` emits RFC 7807 `{type,title,status,detail}` → conflict tolerance unreachable, reconcile never converges.** `internal/controller/dittofs_client.go:90-96, 108-135` (consumed at `auth_reconciler.go:213-214`). `do()` decodes `>=400` bodies into `DittoFSAPIError{Code,Message,Details}` but only returns the typed error when `apiErr.Message != ""` (line 92); otherwise it falls through to a plain `fmt.Errorf` (line 95). The server's only problem constructor, `WriteProblem` (`internal/controlplane/api/handlers/problem.go:43`), sets *exclusively* `Type/Title/Status/Detail` — a repo-wide grep finds **zero** assignments to `Problem.Code` (the `Code` field at `problem.go:28-30` is aspirational/dead) and the JSON never carries a `message` key (it uses `detail`). So `apiErr.Message` always unmarshals to `""`, a typed `*DittoFSAPIError` is essentially never returned, and `IsConflict()`/`IsNotFound()`/`IsAuthError()` — all keyed on `Code == "..."` — are dead. **Blast radius**: the operator-service-account 409 is emitted by `Conflict(w, "User already exists")` (`users.go:114`) → `{"title":"Conflict","status":409,"detail":"User already exists"}`. At `auth_reconciler.go:214`, `errors.As(err,&apiErr)` is false → the "already exists, proceed to login" branch (line 215) is unreachable → `provisionOperatorAccount` returns a hard error on **every** reconcile after the user first exists → the CR never converges. **Why CI is blind**: the operator's own tests mock a fabricated `{"code":"CONFLICT","message":...}` body the real server cannot produce (`auth_reconciler_test.go:211,:314`, `adapter_reconciler_test.go:129`). *Verifier: all six load-bearing claims confirmed independently against the code; HIGH retained.* **Fix**: decode the real problem+json shape (`title`/`detail`/`status`), store `StatusCode` on `DittoFSAPIError`, key `IsConflict`/`IsNotFound`/`IsAuthError` on the status code (409/404/401|403), and always return a typed `*DittoFSAPIError` for `>=400` (drop the `Message != ""` gate). Add a contract test that asserts against real `handlers.Conflict` output, not a hand-mocked body. (Pairs with M-CI-1 below — `StatusCode` is shared between them.)

### Deploy-path security-control gap (liveness-blocking)

- **H2 — Helm chart manager ClusterRole missing `networkpolicies` RBAC required by the security-critical reconciler.** `chart/templates/manager-rbac.yaml` (rules block, `networking.k8s.io/networkpolicies` apiGroup entirely absent). The kubebuilder marker (`dittoserver_controller.go:103`) and `config/rbac/role.yaml:79-90` both grant `networking.k8s.io/networkpolicies {create,delete,get,list,patch,update,watch}`, but the chart RBAC lists only `""`, `apps`, `coordination.k8s.io`, `dittofs.dittofs.com`, and `pgv2.percona.com`. `reconcileNetworkPolicies` (`networkpolicy_reconciler.go:266-267`) explicitly propagates NP errors as "security-critical"; `ensureBaselineNetworkPolicy` (line 146, Get-then-Create at 181) runs early in the main loop (`dittoserver_controller.go:239-242`) on every reconcile. **Blast radius**: a chart-deployed operator's ServiceAccount cannot create/list NetworkPolicies → `ensureBaselineNetworkPolicy` fails with a forbidden error → the reconcile errors every loop → the CR never reaches Ready, and the network-isolation policies that are *presented as the security control* are never applied, so the `dfs` pods run with no NetworkPolicy at all. `docs/INSTALL.md:61-78` documents `helm install ./chart` as a supported path, so this hits the documented install. *Verifier: confirmed by reading all cited locations; this is a real security-control gap plus liveness failure, not cosmetic; HIGH retained.* **Fix**: add a rule for `apiGroups:[networking.k8s.io], resources:[networkpolicies], verbs:[create,delete,get,list,patch,update,watch]` to `chart/templates/manager-rbac.yaml` to match `config/rbac/role.yaml`, and regenerate chart RBAC from the kubebuilder markers (`make manifests`) so the two cannot drift again.

---

## 3. Triage downgrades / RESOLVED

No HIGH was refuted this run. Both reported HIGHs (security H2, client-integration H1) were independently re-verified against the cited source and **retained at HIGH** — the verifier confirmed every load-bearing claim line-by-line (full rationales folded into §2). No sub-audit reported a HIGH that survived adversarial check at a lower severity.

---

## 4. MED findings (terse, grouped by sub-area)

**reconcilers**
- **M-REC-1 — Re-provisioning dead-end: lost credentials Secret + persisted identity store = permanent auth failure (Ready never True).** `auth_reconciler.go:204-225`. When the operator-credentials Secret is absent, `provisionOperatorAccount` generates a *new* random password, swallows the `CreateUser` CONFLICT (line 214), then `Login`s with the new password (222). With a persistent identity backend the existing user still has its *old* password → 401 (non-transient) → `Authenticated=False` permanently. The inverse (user deleted, secret kept) *is* handled in `refreshOperatorToken` (296-303); secret-lost-user-kept is not. Fix: on CONFLICT, admin-reset the existing user's password (admin token is in scope), or detect the operator-Login 401 and delete+recreate the user.
- **M-REC-2 — `Ready` condition not recomputed after `Authenticated` flips True in the same reconcile (one-cycle lag).** `dittoserver_controller.go:232-263`. `updateStatus` (232) computes `Ready` from the pre-`reconcileAuth` `Authenticated` value (auth runs at 246, after); `setAuthCondition` updates only `Authenticated`. On the cycle auth first succeeds, `Ready` stays False until the next reconcile. Self-heals within ~30s via the adapter `RequeueAfter`, but CD health checks gating on `Ready` see a transient false-negative each time auth (re)succeeds. Fix: recompute the aggregate `Ready` after the re-fetch at line 256.

**api-types**
- **M-API-1 — Committed CRD manifests are stale: advertise a `spec.metrics` field that no longer exists in the Go types.** `config/crd/bases/dittofs.dittofs.com_dittoservers.yaml:255-269` and `chart/crds/dittoservers.yaml:255`. Both CRDs declare `spec.metrics{enabled,port}` but `api/v1alpha1/` has no `Metrics` field — `make manifests` was not re-run after the field was deleted. Users can set `spec.metrics` (CRD accepts it) but the operator silently ignores it; OpenAPI validation no longer matches the contract. Fix: `make manifests`, commit the regenerated base CRD, sync `chart/crds/`, and add a CI check that fails on `make manifests generate` diff.

**security**
- **M-SEC-1 — Admin and operator credentials sent over plaintext HTTP to the `dfs` API.** `api/v1alpha1/helpers.go` (`GetAPIServiceURL` hardcodes `http://`), consumed at `auth_reconciler.go:197/222/293/361` via `dittofs_client.go` Login/CreateUser. The bootstrap admin password and the operator service-account password (grants the `operator` role) traverse the pod network in cleartext; RefreshToken/DeleteUser carry bearer tokens in clear. NetworkPolicies restrict ingress to the server, not operator→server sniffing. Fix: serve the control-plane API over TLS and emit `https://` (configurable scheme), or at minimum document the exposure and gate behind a mesh/mTLS requirement.

**client-integration**
- **M-CI-1 — Transient-vs-terminal classifier cannot see HTTP 5xx; a 503/504 from `dfs` is treated as terminal and deletes operator credentials.** `auth_reconciler.go:415-451` (`isTransientError`), used at 298-303. `isTransientError` matches `net.Error`/string patterns only; an HTTP 503 yields `"request failed with status 503: ..."` matching none, and `DittoFSAPIError` carries no `StatusCode`, so the explicit `apiErr` branch (448-450) hard-returns false. A re-login failing with a transient 503 is classified non-transient → the operator deletes the credentials Secret and forces a full re-provision on every transient hiccup. Fix: add `StatusCode` to `DittoFSAPIError` (set in `do()`), treat 502/503/504 (likely 500) as transient. Pairs with H1.

**config-mgmt**
- **M-CFG-1 — Operator emits a top-level `cache:` block that `pkg/config` no longer defines (silent dead config / schema drift).** `config/config.go:21-22,42,142-161`; `config/types.go:10,57-61`. `GenerateDittoFSConfig` always renders `cache:{path,size}`, but `pkg/config.Config` has no `Cache` field (Cache went RAM-only, Phase 16). Viper silently discards unknown keys, so a user setting `spec.cache.*` believes they are sizing/relocating the cache while `dfs` ignores it. A future strict-decode tightening would brick every operator-managed server. Fix: drop `CacheConfig`/`buildCacheConfig` (and the spec field if it exists solely for this); add a round-trip test through `pkg/config.Load`.
- **M-CFG-2 — `admin.username` rendered into the ConfigMap is never read by `dfs` — custom admin username silently inert.** `config/config.go:45-53`. The operator writes `admin.username` (default `"admin"`) but `dfs` bootstrap hard-codes the admin user via `models.AdminUsername` ("admin") in `store.EnsureAdminUser` (`pkg/controlplane/store/users.go:181-218`); nothing reads `cfg.Admin.Username`. A user setting `spec.identity.admin.username="root"` still gets an account named `admin`. Fix: render the fixed `admin` (and document), or teach `dfs` to honor `cfg.Admin.Username`; add a round-trip assertion.
- **M-CFG-3 — No shared source of truth and no round-trip test between operator config schema and `pkg/config` (drift is structurally invisible).** `config/types.go:1-67` (whole file); the `config/` dir has zero `_test.go`. `types.go` hand-mirrors the `dfs` schema with its own yaml tags; the operator never imports `dittofs/pkg/config`, and no test feeds `GenerateDittoFSConfig` output into `config.Load`. This is the **root cause** of M-CFG-1/M-CFG-2 and the highest-leverage future-brick risk: a key rename in `pkg/config` would mis-configure every managed server with no build/test signal. Kept MED only because today's emitted keys still parse. Fix: add a test that round-trips `GenerateDittoFSConfig` through `pkg/config.Load` with a strict/`ErrorUnused` decoder; best, import `pkg/config` and reuse its structs instead of re-declaring them.

---

## 5. LOW findings (terse, grouped)

**reconcilers**
- **L-REC-1** — `lastKnownAdapters` in-memory map leaks one entry per deleted `DittoServer`; `handleDeletion`/`performCleanup` (`dittoserver_controller.go:457-579`) never `delete(r.lastKnownAdapters, ns+"/"+name)`. (`adapter_reconciler.go:137-147`.) Unbounded slow growth over operator lifetime. Fix: delete under `adaptersMu` in `handleDeletion`.
- **L-REC-2** — `setRetryCount(0)` issues an `Update` on every successful reconcile even when the annotation is already absent (`auth_reconciler.go:491-497`). No hot-loop (apiserver suppresses no-op resourceVersion bump) but an avoidable per-cycle round-trip on the hot path. Fix: only `Update` when the annotation actually changes.
- **L-REC-3** — `ensurePortmapperEnabled` detects the NFS adapter via `isNFSAdapter(sanitize(a.Type))` but calls the hardcoded `/api/v1/adapters/nfs/settings` path (`adapter_reconciler.go:106-134`; `dittofs_client.go:232,243`). Coupling sanitized detection to a hardcoded REST segment is fragile; best-effort path so low-risk today. Fix: derive the path from `adapter.Type` or assert the `nfs` segment invariant.

**api-types**
- **L-API-1** — `dittoserver_types_builder.go:1-226` is test-only functional-option scaffolding shipped in the production `api/v1alpha1` package; 17 of 28 `New*`/`With*` builders (`WithImage`, `WithControlPlane`, `WithIdentity`, … `WithConditions`) are entirely dead, the other 11 used only by tests. Fix: move test-used builders to `_test.go`/internal testutil; delete the 17 unused.
- **L-API-2** — `helpers.go:43` `HasUserProvidedJWTSecret` is an unused exported method on the public API type (the inline predicate is used directly inside `GetEffectiveJWTSecretRef`). Fix: delete or reuse it.
- **L-API-3** — Managed JWT secret name duplicated as a string literal: `GetEffectiveJWTSecretRef` builds `ds.Name + "-jwt-secret"` (`helpers.go:37`) while the controller re-derives `dittoServer.Name + "-jwt-secret"` (`dittoserver_controller.go:639`). Two sources of truth; a future rename of one breaks auth. Fix: add a `GetManagedJWTSecretName()` helper used by both.

**security**
- **L-SEC-1** — Adapter and baseline NetworkPolicies set `PolicyTypes:[Ingress]` with an Ingress rule that has `Ports` but no `From` peer (`networkpolicy_reconciler.go:99-120,155-172`). Per K8s semantics empty `From` allows *all* sources on the listed ports — correct default-deny-on-other-ports for a public fileserver, but the "security-critical/restrictive isolation" doc comments overstate it. Not a bug. Fix: document that source-level restriction is out of scope, or add an optional `Ingress.From` scoping field.

**config-mgmt**
- **L-CFG-1** — Operator injects `DITTOFS_ADMIN_PASSWORD_HASH` env var that `dfs` neither binds via viper nor reads at bootstrap (`dittoserver_controller.go:1114-1123`). When `spec.identity.admin.passwordSecretRef` is set, the StatefulSet injects the hash, but `EnsureAdminUser` only reads `DITTOFS_ADMIN_INITIAL_PASSWORD` via `models.GetOrGenerateAdminPassword` — the user-supplied hash is dropped and a random password is generated. Fix: either `BindEnv("admin.password_hash")` + honor it in `dfs`, or deliver via the supported `DITTOFS_ADMIN_INITIAL_PASSWORD` path; add an env-name contract test.

**simplicity-conventions**
- **L-SIMP-1** — 55 MB prebuilt `manager-linux-amd64` ELF committed to git (`k8s/dittofs-operator/manager-linux-amd64`, added in commit `915deff5`/#113). It is `.gitignore`d (line 82) but gitignore does not untrack already-tracked files, so it ships in every clone and bloats history permanently. Only `Dockerfile.prebuilt` consumes it. Fix: `git rm --cached` it (keep the gitignore entry); build in CI or drop `Dockerfile.prebuilt`.
- **L-SIMP-2** — (= L-API-1) test-only fluent builder (~225 LOC) shipped in public `api/v1alpha1`; move to internal test helper.
- **L-SIMP-3** — `utils/conditions/conditions.go:32-101` reimplements status-condition CRUD with a generic `ConditionType[~string]` constraint, duplicating canonical `k8s.io/apimachinery/pkg/api/meta` `SetStatusCondition`/`RemoveStatusCondition`/`FindStatusCondition`/`IsStatusConditionTrue` (already in the dep tree). All 22 callers pass plain string constants, so the generics buy nothing and the custom impl has subtly different `LastTransitionTime` semantics. Fix: replace with `meta.*` and delete the package.
- **L-SIMP-4** — Generated `Dockerfile.cross` committed to git, but the `Makefile` `docker-buildx` target generates it via `sed` (line 136) and `rm`s it (line 141). Fix: `git rm` it, add to `.gitignore`.
- **L-SIMP-5** — (= L-REC-1) per-CR `lastKnownAdapters` map never frees entries on deletion.
- **L-SIMP-6** — Planning-doc IDs leaked into source comments: `CONTEXT.md` (`dittoserver_controller.go:1244,1294`; `config/config.go:64,79`) and `DISC-03` (`adapter_reconciler.go:73`, `networkpolicy_reconciler.go:277`, `service_reconciler.go:219`). Violates `feedback_no_phase_comments_in_code`. Fix: reword to state the actual behavior, drop the IDs.
- **L-SIMP-7** — `config/samples/` has 9 CR YAMLs under three naming schemes (`ditto.io_v1alpha1_*`, `dittofs_v1alpha1_*`, `dittofs-*`); `kustomization.yaml` references only 3, and the `ditto.io_` prefix is stale (files correctly declare `dittofs.dittofs.com/v1alpha1`). Fix: consolidate to one prefix, delete/wire orphans.
- **L-SIMP-8** — Dead/test-only exported surface: `HasUserProvidedJWTSecret` (= L-API-2) and `pkg/resources/service.go:86` `ServiceBuilder.AddTCPPortWithTarget` (called only by its own unit tests). Fix: delete (with tests) or confirm a future caller.
- **L-SIMP-9** — `PROJECT` scaffold metadata stale: declares `version: v1` and path `.../api/v1`, but the real package is `api/v1alpha1`. Misleads kubebuilder `create`/`edit` codegen and readers about API maturity. Fix: correct to `v1alpha1` / `.../api/v1alpha1`.

---

## 6. Verified-correct (checked and found OK)

**reconcilers**
- Idempotency: every child resource (ConfigMap, JWT/admin/credentials Secrets, headless+API Services, StatefulSet, PerconaPGCluster, baseline+adapter NetworkPolicies, adapter Services) uses `SetControllerReference` + `CreateOrUpdate` or Get-then-Create/Update with `IsAlreadyExists`/`IsNotFound` — no blind Create.
- Finalizer cleanup does not deadlock: `handleDeletion` (`dittoserver_controller.go:457-509`) requeues 5s on cleanup error and force-removes the finalizer after `cleanupTimeout=60s`; `performCleanup` orphans-or-deletes Percona per `deleteWithServer`.
- Requeue/error handling: transient errors requeue with capped exponential backoff (`computeBackoff`, 2s..5m, overflow-guarded at `retryCount>20`) and return nil; permanent auth failures return the error; `CreateOrUpdate` optimistic-lock conflicts retried via `retryOnConflict` (3 attempts, `IsConflict`-gated) — no terminal-error hot-loop.
- Concurrency: `MaxConcurrentReconciles` unset (default 1) so reconciles serialize per controller; `lastKnownAdapters` additionally guarded by `adaptersMu` RWMutex — race-free.
- Service merge (`mergeServiceSpec`/`mergePorts`/`mergeAnnotations`, `1383-1455`) preserves cloud-controller-owned fields (`ClusterIP`, `NodePort`, `HealthCheckNodePort`); port/NetworkPolicy reconciliation only `Update`s when ports actually change (avoids needless StatefulSet rolling restarts).
- Baseline NetworkPolicy ensured before adapter NPs (main `Reconcile:239` and inside `reconcileNetworkPolicies:273`) — avoids default-deny chicken-and-egg blocking operator→API traffic. NP errors propagated (security-critical) while adapter-Service errors best-effort — a deliberate, reasonable split.
- `refreshOperatorToken` handles user-deleted-but-secret-present by deleting the stale Secret on non-transient re-login failure (`296-303`).

**api-types**
- `zz_generated.deepcopy.go` regenerates byte-identically with controller-gen v0.19.0 — **no stale-deepcopy / shared-pointer aliasing bug**; every pointer/slice/map field and embedded k8s struct (`ResourceRequirements`, `SecurityContext`) deep-copies correctly.
- Webhook validation is minimal-but-sane and well-tested (storage required, JWT key-requires-name, port range, Percona/backup required fields, StorageClass existence); kubebuilder markers (`subresource:status`, printcolumns, enums, min/max, patterns, defaults) present and correct.

**security**
- RBAC least-privilege: no cluster-admin, no wildcard verbs on core resources; the only `*` is scoped to the `dittoservers` CRD in the standard aggregated-admin role.
- Secrets never logged, never in CR status (only SHA256 `ConfigHash`) or events; all credentials injected via `SecretKeyRef`. JWT/admin/operator passwords use `crypto/rand` (`generateRandomSecret`, with error propagation).
- Webhook served over TLS via the controller-runtime webhook server with a cert watcher and HTTP/2 disabled.

**client-integration**
- HTTP timeout `10s` (`dittofs_client.go:46`); `do()` uses `http.NewRequestWithContext` (67); reconcilers thread the reconcile ctx (cancellation on shutdown works).
- TLS verification ON by default (no custom transport, no `InsecureSkipVerify`); in-cluster `http://` makes verification N/A here — acceptable design.
- Endpoint paths/payloads match server routes and `pkg/apiclient`: `/auth/login`, `/auth/refresh`, `/users`, `/users/{username}`, `/adapters`, `/adapters/nfs/settings`. Login/RefreshToken/CreateUser/ListAdapters/GetNFSSettings/EnablePortmapper shapes all match canonical. `DeleteUser` path is unescaped but its only caller passes the fixed `OperatorServiceAccountUsername` constant — no traversal. Response body fully read and closed (no leak).

**config-mgmt**
- Secrets NOT baked into the ConfigMap: `reconcileConfigMap` writes only `GenerateDittoFSConfig` output; JWT secret, admin password hash, and Postgres DSN/password inject as env vars; the Postgres block renders placeholder values only.
- JWT secret path correct (`DITTOFS_CONTROLPLANE_SECRET`, read via `os.Getenv`, env precedence over config). Postgres env-var names align with `dfs` `BindEnv` (`DITTOFS_DATABASE_POSTGRES_*`); precedence logic sound (`type=postgres` + SQLite cleared when `PostgresSecretRef` present). Controlplane/JWT key names match the canonical schema; `database` sub-keys match via mapstructure case-insensitive field matching despite no struct tags. Consumed defaults (`shutdown_timeout 30s`, logging level INFO) align; logging format/output are deliberate container overrides.

**simplicity-conventions**
- Watch wiring idiomatic (`For` + `Owns` + conditional `Owns(PerconaPGCluster)` gated on RESTMapping). `bin/controller-gen-v0.19.0` (33 MB) correctly gitignored and untracked. The hand-rolled `DittoFSClient` is justified bloat-avoidance (6 endpoints, proper ctx + body Close). `RequeueAfter` polling for adapter discovery is correct (adapters are REST sub-resources, not k8s objects — watches cannot apply). `retryOnConflict` + `CreateOrUpdate` + `SetControllerReference` used consistently; finalizer handling idiomatic with a force-removal escape hatch. Standard kubebuilder layout intact.

---

## 7. Recommended PR-B shape

Split the two HIGHs into focused fix PRs; defer MED/LOW as tracked issues.

- **PR-B1 — operator↔server error-contract fix (H1 + M-CI-1).** Decode RFC 7807 problem+json in `dittofs_client.go`; add `StatusCode` to `DittoFSAPIError`; re-key `IsConflict`/`IsNotFound`/`IsAuthError` on status code; always return typed `*DittoFSAPIError` for `>=400`; treat 502/503/504/500 as transient in `isTransientError`. **Replace the fabricated `{code,message}` test mocks** with a contract test asserting against real `handlers.Conflict`/`WriteProblem` output. *Highest priority — unblocks reconcile convergence.*
- **PR-B2 — chart RBAC drift fix (H2).** Add the `networking.k8s.io/networkpolicies` rule to `chart/templates/manager-rbac.yaml`; regenerate chart RBAC from kubebuilder markers; add a CI check that fails if `config/rbac` and `chart/` RBAC diverge (or generate the chart RBAC from the markers so divergence is impossible).
- **PR-B3 — config-schema contract + drift cleanup (M-CFG-1/2/3).** Add a round-trip test through `pkg/config.Load` with a strict decoder (ideally import `pkg/config` and reuse its structs); delete the dead `cache:` block and the inert `admin.username` knob (or wire them server-side). Fold in L-CFG-1 (`ADMIN_PASSWORD_HASH`).
- **PR-B4 — CRD/manifest regen + CI gate (M-API-1).** `make manifests`, commit regenerated base CRD, sync `chart/crds/`, add a `make manifests generate` no-diff CI check.
- **PR-B5 — status + auth-recovery correctness (M-REC-1, M-REC-2).** Recompute `Ready` after auth in the same reconcile; add an admin-reset/recreate recovery path for lost-credentials-Secret + persistent-identity.
- **PR-B6 — plaintext-credentials TLS (M-SEC-1).** Larger; can land independently or be deferred to a v1.0 follow-up if mTLS/mesh is documented as the interim mitigation.
- **Defer as issues**: all 17 LOW. Bundle the repo-bloat/convention cluster (L-SIMP-1/3/4/6/7/9, L-API-1/2/3) into one cleanup PR if desired — high count, low individual risk. The 55 MB committed binary (L-SIMP-1) is worth its own small PR given the per-clone cost.

---

## 8. Coverage

**Audited**: all of `k8s/dittofs-operator/` — `api/v1alpha1/` (types, webhook, builder, helpers, `zz_generated.deepcopy`), all five reconcilers + `dittofs_client.go` + `config/`, `chart/` (RBAC + CRD), `config/` (RBAC, CRD bases, samples, kustomization), `utils/conditions/`, `pkg/resources/`, `cmd/main.go`, `PROJECT`, `Makefile`, Dockerfiles, and the committed `manager-linux-amd64` blob. Cross-checked against the `dfs` server (`internal/controlplane/api/handlers/problem.go`, `users.go`, `pkg/config`, `pkg/controlplane/store`, `pkg/apiclient`, `models.AdminUsername`) and against controller-runtime / kubebuilder / `apimachinery/meta` canonical patterns. Deepcopy correctness verified by regenerating with controller-gen v0.19.0 and diffing.

**Not audited (out of scope for PR-A correctness/design)**:
- **Live cluster behavior** — no deploy to Kapsule/`dittofs-demo` was exercised; findings are static-analysis + contract-diff. The H1/H2 liveness claims are reasoned from code, not observed on a running cluster (though both are deterministic given the cited code).
- **Percona PGCluster operator interaction** beyond the operator's own create/orphan logic — the upstream Percona operator's reconcile is third-party and unaudited.
- **Webhook cert lifecycle / cert-manager wiring** at runtime (the server-side TLS plumbing was verified to exist; rotation under load was not exercised).
- **Performance/scale** — no reconcile-throughput or large-fleet (`lastKnownAdapters` growth at scale) profiling; L-REC-1/L-SIMP-5 leak is reasoned, not measured.
- **Helm chart values matrix / upgrade paths** — only the RBAC template and CRD were diffed; the full `values.yaml` surface and chart-upgrade migrations were not audited.
