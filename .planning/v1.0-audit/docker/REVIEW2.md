# Area: docker — Round-2 Audit (REVIEW2.md)

**Status**: AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Date**: 2026-06-01.
**Branch**: docker audit run on `v1.0/blockstore-perf-b1` tree (= `develop` + B1/B2/B3 perf). PR-B to be opened off `develop`.
**Scope**: Production images — root `Dockerfile` (80L), `Dockerfile.goreleaser`, `Dockerfile.prebuilt`, `.dockerignore`; operator `k8s/dittofs-operator/Dockerfile{,.cross,.prebuilt,.dockerignore}`; release surface `.goreleaser.yml` + `.github/workflows/release.yml`; operator controller-rendered PodSpec/CRD; test harness `test/{nfs,smb}-conformance/*`, `test/posix/*`, `test/integration/{kerberos,portmap}/*` + their docker-compose. dfs ports 12049 NFS / 12445 SMB / 8080 REST.
**Cross-check refs**: Samba `samba-team/samba`, Linux `torvalds/linux/fs/nfs` (no protocol-relevant docker findings surfaced); Kubernetes pod-termination semantics (preStop → SIGTERM → grace → SIGKILL); cosign / SLSA provenance + SBOM (syft) supply-chain norms; round-1 `11-operator/REVIEW.md` (re-verified, see §3/§8).

**Method**: 5 parallel read-only sub-audits — image-security, build-correctness, runtime-config/PID-1, supply-chain/bloat, compose/test-parity. This is a NEW area (no prior docker-specific REVIEW exists; the round-1 `11-operator` pass touched operator Go/Dockerfile mechanics but never assessed image signing, SBOM, digest-pinning, or the operator→server shutdown-grace boundary). The single HIGH was independently adversarially re-verified against current develop.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| image-security | 0 | 1 | 3 |
| build-correctness | 0 | 2 | 4 |
| runtime-config / PID-1 | 1 | 1 | 3 |
| supply-chain / bloat | 0 | 3 | 4 |
| compose / test-parity | 0 | 1 | 5 |
| **Total (post-triage, deduped)** | **1** | **5** | **14** |

> The five sub-audits independently re-discovered the same digest-pinning, Go-toolchain-drift, committed-57MB-binary, `Dockerfile.cross`-generated, and `9090`-EXPOSE-drift items. The tally above is the raw per-sub-area count; §4/§5 present these **deduped** into one finding each with all cross-citations merged.

**Architecture invariants: clean.** Every production image runs as fixed non-root `65532:65532`; no secret, key, keytab, `.env`, or secret build-arg is baked into any layer; release tokens (`HOMEBREW_TAP_TOKEN`, `SCOOP_BUCKET_TOKEN`) and cosign keyless OIDC live only in the release env; `COPY` scope strips `.git`/secrets/configs/tests; embedded postgres `*.sql` migrations are correctly NOT `.dockerignore`'d so the in-container build doesn't break; PID-1 signal handling is correct in both the dfs server (exec-form ENTRYPOINT → real graceful shutdown chain) and the operator manager (`ctrl.SetupSignalHandler()`). No CLAUDE.md invariant is violated by the container layer.

**Verdict: NEEDS-FIX (single, narrow, real).** One HIGH — a Kubernetes **operator→server shutdown-grace boundary** defect that can SIGKILL dfs mid-metadata-flush on any rollout/scale-down/node-drain = metadata loss. It is exactly the cross-boundary, error-path class this round-2 pass was chartered to catch: invisible in round-1 because the operator audit looked at the controller in isolation and the server audit looked at shutdown in isolation, but the two independently-chosen timeouts (k8s default 30s grace vs. dfs per-stage `shutdown_timeout` hardcoded to 30s) collide only when viewed across the seam. Everything else is supply-chain hardening (no SBOM/image-signature/provenance, mutable-tag bases) and reproducibility/bloat/drift hygiene. Absent the one HIGH this area is **PATCH-grade**; the HIGH is contained to the operator-managed deployment path (standalone `docker stop` is fine), so it is a focused fix rather than a broad rewrite.

---

## 2. HIGH findings (triaged, ranked by blast radius)

### Liveness / data-loss at the operator↔server boundary

- **H1 — Operator dfs StatefulSet sets no `terminationGracePeriodSeconds` (k8s default 30s) while the dfs serial shutdown chain budgets up to 30s *per stage* → SIGKILL mid-flush = metadata loss on rollout.** `k8s/dittofs-operator/internal/controller/dittoserver_controller.go:943-1024` (PodSpec; PreStop at :999, no TGPS field anywhere).
  - **What**: The operator renders the dfs server PodSpec with a PreStop hook `["/bin/sh","-c","sleep 5"]` (:999) but never sets `PodSpec.TerminationGracePeriodSeconds`, so Kubernetes applies its 30s default. preStop runs *inside* the grace window before SIGTERM, leaving ~25s. The dfs graceful shutdown (`pkg/controlplane/runtime/lifecycle/service.go:217-281`) runs **serially**, each major stage independently bounded by `s.shutdownTimeout`: snapshot-drain (`:251-253`), `StopAllAdapters`, `FlushAllPendingWritesForShutdown` (`:262`), `CloseMetadataStores` (`:271`), `apiServer.Stop` (`:275`). A single flush stage can consume the full budget.
  - **Why HIGH**: The default `ShutdownTimeout` is 30s (`pkg/config/defaults.go:60-61`, `pkg/controlplane/runtime/runtime.go:24` `DefaultShutdownTimeout=30s`), and the operator **hardcodes** `shutdown_timeout: 30s` into the rendered dfs `config.yaml` (`k8s/dittofs-operator/internal/controller/config/config.go:15` applied at `:39`) with **no CRD knob to lower it** (`api/v1alpha1/dittoserver_types.go` has no grace/termination/shutdown field). So the server's per-stage budget (30s) **deterministically exceeds** the ~25s effective k8s grace. On a rolling update / scale-down / node-drain, a `FlushAllPendingWritesForShutdown` stalling on a slow badger/postgres backend is SIGKILLed mid-flush → unflushed metadata writes are lost. Worst case (drain + flush + api-stop all stalling) is ~3× the budget. Standalone `docker stop` and the foreground signal path are fine — the loss is specific to the operator-managed k8s path.
  - **Fix**: Set `PodSpec.TerminationGracePeriodSeconds` to comfortably exceed `preStop(5s) + worst-case shutdown stages` (at minimum `preStop + shutdown_timeout + margin`; ideally `preStop + 3×shutdown_timeout`). **Derive it from the configured `shutdown_timeout`** so the two are coupled rather than independently chosen, and add a CRD field so operators can tune it. Out-of-band StatefulSet edits are reconciled back, so this must be fixed in the controller.
  - **Verifier rationale**: CONFIRMED at the cited code; finding is in fact **understated**. `grep TerminationGracePeriodSeconds` in `internal/` returns nothing for the dfs StatefulSet (the 10s TGPS in `chart/templates/deployment.yaml` / `config/manager/manager.yaml` is the operator's *own* manager pod, not the dfs pod). The operator pins `shutdown_timeout` to exactly 30s with no override path, so the collision is deterministic, not merely possible. Citation path imprecisions in the raw finding (`lifecycle/service.go` → actual `runtime/lifecycle/service.go`; `defaults.go` → `pkg/config/defaults.go`) are cosmetic — every cited behavior was independently verified at the real locations. HIGH stands.

---

## 3. Triage downgrades / RESOLVED

None. No sub-audit raised a HIGH that was refuted; the single HIGH (H1) was adversarially verified and held (in fact strengthened). No round-1 HIGH exists in this area to re-verify (round-1 `11-operator` carried only LOW items — the committed binary and `Dockerfile.cross` — both of which still reproduce on current develop and are carried forward in §4/§5, not as RESOLVED). No shipped PR since round-1 closes any docker finding.

---

## 4. MED findings (terse, deduped)

**Supply-chain**

- **M1 — No SBOM or image-level signature/provenance for any published image (server *or* operator).** `.goreleaser.yml:141-149` (cosign signs only `checksums.txt`, a blob), `:177-200` (`dockers_v2` — no `sbom:`/provenance); `.github/workflows/release.yml:197-204` (operator `docker/build-push-action` — no `provenance:`/`sbom:`/sign), and the operator build job (`release.yml:162-164`) declares only `contents: read` so it *couldn't* sign. Images are the primary deploy artifact (`docker run marmos91c/dittofs`, the k8s operator), yet downstream has no `cosign verify` for the image and no bill-of-materials for CVE scanning. **Highest-leverage gap; round-1 never assessed it.** Fix: add goreleaser `sboms:` (syft), sign published images (`cosign sign <image>@<digest>` + `cosign attest --predicate sbom`), set `provenance: true`/`sbom: true` on the operator build-push step, grant it `id-token: write`, and document image verification in the release footer.

- **M2 — Base images pinned by mutable tag, not `@sha256` digest (all 7 Dockerfiles).** `Dockerfile:5,34`; `Dockerfile.goreleaser:4`; `Dockerfile.prebuilt:1`; operator `Dockerfile:2,27`, `Dockerfile.cross:2,27`, `Dockerfile.prebuilt:3` — `golang:1.25-alpine`, `alpine:3.21`, `golang:1.24.3`, `gcr.io/distroless/static:nonroot` (a rolling tag). Builds are non-reproducible and silently absorb a re-pushed/compromised upstream tag; combined with M1 there is no record of what base layer shipped. Better than `:latest` (already avoided) but short of production best practice. Fix: pin every `FROM` by digest (keep the human tag in a comment); refresh via Renovate/Dependabot. Prioritize runtime bases (`alpine:3.21`, `distroless:nonroot`), then the golang builders.

- **M3 — 57 MB prebuilt `manager-linux-amd64` ELF committed to git.** `k8s/dittofs-operator/manager-linux-amd64` (tracked since `915deff5`, #113); root `.gitignore:82` lists it but git does not untrack already-tracked files; operator `.dockerignore:14` re-includes it via `!manager-*`. Consumed only by `Dockerfile.prebuilt`, but the from-source `Dockerfile`/`Dockerfile.cross` do `COPY . .` (`:16`) so the 57 MB is dragged into every build context + builder layer; it is also a permanent history blob and an unauditable, provenance-less supply-chain artifact (round-1 flagged it LOW; for a 57 MB opaque ELF, MED is warranted). Fix: `git rm --cached` it (keep the `.gitignore`); build in CI; if `Dockerfile.prebuilt` is only an escape hatch, delete it (see L7).

**Build-correctness**

- **M4 — Operator builder `golang:1.24.3` is older than its `go.mod` `go 1.25.1` → non-hermetic toolchain download at build time.** operator `Dockerfile:2` / `Dockerfile.cross:2` vs `k8s/dittofs-operator/go.mod:3`. Only succeeds because `GOTOOLCHAIN=auto` (`Dockerfile:13,23`) fetches 1.25.1 from `proxy.golang.org`/`dl.google.com` mid-build — fails outright under `GOTOOLCHAIN=local` / air-gapped CI, makes the pinned base misleading, and adds an unpinned network hop to the supply chain. The root server Dockerfile correctly uses `golang:1.25-alpine`. Fix: bump operator builders to `golang:1.25(-alpine)` matching the module; keep `GOTOOLCHAIN=auto` as a fallback only.

**Test-parity**

- **M5 — `test/integration/kerberos/Dockerfile:6-14` broken `apt` retry can silently build an image with krb5 missing.** Under `set -e`, a command used as an `if` condition is exempt from errexit, so a failing `apt-get` does NOT abort; if all three retries fail (the exact transient-network case the loop exists for), the loop completes, `rm -rf` succeeds, RUN exits 0 — yielding a "successful" image missing `krb5-kdc`/`krb5-admin-server`, surfacing later as an opaque `kadmin not found` entrypoint error. The three sibling KDC/nfs-client Dockerfiles use the correct hard-fail `... && exit 0; sleep 5; done; echo ERROR; exit 1`. Test-harness only (hence MED), but defeats the retry's purpose and produces flaky-looking, hard-to-debug integration runs. Fix: adopt the sibling idiom.

---

## 5. LOW findings (terse, grouped)

**Hygiene / bloat**

- **L1 — Final Alpine server images ship busybox shell + `apk` + wget (attack surface vs distroless).** `Dockerfile:34,45`; `Dockerfile.goreleaser:4,15`; `Dockerfile.prebuilt:1,2`. The operator already uses `distroless/static:nonroot` with none of these; the binary is static (`CGO_ENABLED=0`). `wget` is used only by the HEALTHCHECK; a quick grep found no `time.LoadLocation`/tzdata use, so `tzdata` may also be unnecessary. Fix: migrate server runtime to distroless, replace the wget HEALTHCHECK with a `dfs healthcheck` subcommand, drop `tzdata` if confirmed unused — or stay on Alpine but pin by digest.
- **L2 — Root `.dockerignore:30-35` does not exclude host-built `dfs`/`dfsctl` (~60MB/~17MB) or `.DS_Store`.** Only `dittofs`, `*.exe`, `*.dll`, `*.so`, `*.dylib` are excluded; root `Dockerfile` does `COPY . .`. No final-image leak (builder discarded, binary recompiled) — build-context bloat only. Fix: add `dfs`, `dfsctl`, `.DS_Store`, or invert to an allow-list like the operator's.
- **L3 — Operator `go build -a` forces full stdlib rebuild, defeating cache.** operator `Dockerfile:23` / `Dockerfile.cross:23`. Pure build-time cost; static linking is already guaranteed by `CGO_ENABLED=0`; the root Dockerfile correctly omits `-a`. Fix: drop `-a`.
- **L7 — Root `Dockerfile.prebuilt` is orphaned: `COPY dfs-linux-amd64` (uncommitted, produced by nothing) + stray `EXPOSE 9090`.** `Dockerfile.prebuilt:6,10`. `dfs-linux-amd64` is referenced by no build target/Makefile/CI (grep-zero outside this file), so the file is un-buildable dead surface. Fix: delete it (the release path uses `Dockerfile.goreleaser`, local builds use `Dockerfile`); if kept, document the binary source and drop/justify 9090.
- **L8 — Committed operator `Dockerfile.cross` is a Makefile-generated-then-`rm`'d artifact.** `k8s/dittofs-operator/Makefile:136` (sed-generates), `:141` (`rm`). The committed copy is a stale snapshot that can silently drift from `Dockerfile` (it currently shares the 1.24.3↔1.25.1 mismatch). Round-1 L-SIMP-4; still present. Fix: `git rm` it and `.gitignore` it.

**Drift / parity**

- **L4 — Go toolchain version drift between server (`golang:1.25-alpine`) and operator (`golang:1.24.3`) images shipped together.** `Dockerfile:5` vs operator `Dockerfile:2`. Two compiler/stdlib (incl. crypto/TLS) levels for the two halves of one release; papered over by `GOTOOLCHAIN=auto`. Fix: standardize both on `golang:1.25`, consider a pinned `GOTOOLCHAIN`. (Same root cause as M4, viewed as cross-image drift.)
- **L9 — `Dockerfile.prebuilt:10` EXPOSEs `9090/tcp` that nothing binds.** Control-plane binds only `:8080` (`pkg/controlplane/api/config.go:67`, `server.go:87`); no metrics/9090 listener exists in `pkg/cmd/internal`. `Dockerfile` and `Dockerfile.goreleaser` correctly omit it. Cosmetic EXPOSE drift confined to the non-release fallback. Fix: remove `9090/tcp`, or wire a real metrics endpoint if intended. (Surfaced independently by three sub-audits; same line as L7's EXPOSE.)
- **L10 — `test/integration/portmap/Dockerfile:3` uses unpinned `golang:alpine`** (vs `golang:1.25-alpine` everywhere else) — a future tag bump silently changes the toolchain the portmap test compiles `dfs`/`dfsctl` with. Test-only reproducibility. Fix: pin to `golang:1.25-alpine`.
- **L11 — `test/posix/Dockerfile.pjdfstest:24` clones upstream `pjdfstest` master at `--depth 1`** — moving tip, no record of which revision was tested; a red run could be upstream drift, not a DittoFS regression. Fix: pin to a commit/tag.
- **L12 — `test/smb-conformance/Dockerfile.dittofs:35,41,61` comment claims wget but installs unused `curl`; SMB uses wget while NFS variant uses curl for the identical `/health/ready` probe.** Cosmetic parity drift + an unused curl layer. Fix: normalize both conformance images on one tool.

**App-behavior / config-contract**

- **L5 — Read-only-rootfs: GC fallback + a config default write under `os.TempDir()` (`/tmp`), not a VOLUME.** `pkg/blockstore/engine/gc.go:657` (`os.MkdirTemp("","dittofs-gc-")`), `pkg/config/config.go:741` (`filepath.Join(os.TempDir(),"dittofs")`). Image declares VOLUMEs only for `/data/*` and `/config`; under `securityContext.readOnlyRootFilesystem: true`, `/tmp` is unwritable unless an `emptyDir` is mounted. Normal operation (writes to `/data`,`/config`) is read-only-rootfs compatible — only fallback/default modes hit it. Fix: document/require a writable `/tmp` emptyDir, declare a tmpfs VOLUME, or point the fallback under `/data`.
- **L6 — Default CMD forces `--config /config/config.yaml`; `MustLoad` hard-fails when absent, so `DITTOFS_*` env-only config cannot boot the stock image.** `pkg/config/config.go:497-502` vs `Dockerfile:80` / `Dockerfile.goreleaser:50`. Because `configPath` is non-empty, `MustLoad` `os.Stat`s the explicit path and returns "configuration file not found" before viper ever consults the `DITTOFS_*` env layer (`Load` at `config.go:441` is never reached); `/config` is a VOLUME shipping no file. `docker run dittofs` with no mounted config exits immediately with a CLI-oriented "run dittofs init" message. (Captured as a runtime MED in its sub-audit; deduped to LOW here as a container-UX/config-contract gap, not data loss. **Confirm intended behavior with the config-surface auditor, cross-area #9.**) Fix: fall through to env+defaults when the explicit path is absent, or drop `--config` from the default CMD.
- **L13 — Operator preStop hook hard-depends on `/bin/sh`** (`dittoserver_controller.go:999`). Works on the shipped alpine dfs images; `dittoServer.Spec.Image` is operator-configurable, so a distroless/scratch dfs image breaks the blind 5s drain sleep silently. Fix: prefer a probe-based drain gate, or document the `/bin/sh` requirement. (Coupled to H1's preStop and L1's distroless suggestion.)
- **L14 — Non-foreground `dfs start` as PID 1 forks a setsid daemon then exits, killing the container.** `cmd/dfs/commands/daemon_unix.go:80-104`. Shipped CMDs all include `--foreground` so the built images are safe; this is a latent footgun if CMD is overridden, with no PID-1/container guard. Fix: detect PID-1/container context and force foreground or error clearly.
- **L15 — Hardcoded test credentials in compose env + KDC entrypoints** (`test/smb-conformance/docker-compose.yml:43-44`, `test/nfs-conformance/docker-compose.yml:21-22`, KDC entrypoints). Acceptable test-only fixtures in isolated ephemeral containers; flagged per scope. Sub-item: `test/integration/kerberos/entrypoint.sh:39` passes the master password on the command line (`/proc/cmdline` exposure) whereas the conformance KDC entrypoints deliberately pipe it via stdin. Fix: none required; optionally align the integration entrypoint to the stdin pattern.

---

## 6. Verified-correct (checked and found OK)

**Image security / non-root**
- All four production server images (root `Dockerfile:46-47,59`, `Dockerfile.goreleaser:15-17,30`, `Dockerfile.prebuilt:2-4,9`) and all three operator images (`Dockerfile:30`, `.cross:30`, `.prebuilt:5`) run as fixed non-root `65532:65532` with a dedicated `dittofs` group — no image runs as root; operator sets `PodSecurityContext.FSGroup=65532` (controller `:1036-1044`) so mounted PVs are group-writable (the classic k8s PV-ownership pitfall is handled).
- Operator images use `gcr.io/distroless/static:nonroot` — minimal, shell-less, non-root.
- No secrets/keys/keytabs/`.env`/credential files exist in the repo root or are COPYed into any layer; no secret build-args (only non-sensitive `VERSION/COMMIT/BUILD_DATE/TARGET*`); release tokens + cosign keyless OIDC live only in the release env.
- No `:latest` in any production Dockerfile.
- All EXPOSEd ports (12049/12445/8080/9090) are >1024 → non-root binary needs no `CAP_NET_BIND_SERVICE`/setcap/setuid; no capabilities added in any Dockerfile. (SMB conformance harness correctly scopes `cap_net_bind_service` for its port-445 test only.)

**Build correctness**
- Multi-stage hygiene clean: builder (`golang:*-alpine` + git/ca-certs/tzdata) fully discarded; final images carry only the static `CGO_ENABLED=0` binary + ca-certs + tzdata + non-root user. No compilers/git/source leak into final images.
- `COPY` scope safe: root `.dockerignore` strips `.git`/`.github`/configs/`test/`/`*_test.go`/docs/`CLAUDE.md`; operator `.dockerignore` denies-all (`**`) then re-includes only `**/*.go` (minus `*_test.go`)/`go.mod`/`go.sum`/`manager-*` — `COPY . .` cannot drag in `.git` or credentials.
- Embedded postgres migrations build correctly in-container: `pkg/metadata/store/postgres/migrations/embed.go` `//go:embed *.sql` and `.dockerignore` does not exclude `*.sql`; no other go:embed could be starved.
- Healthcheck is real and reachable: `GET /health` registered **unauthenticated** before the JWTAuth group (`router.go:94-96`), API binds `:8080` on all interfaces, started **unconditionally** during `dfs start` (`start.go:238`); busybox wget present in alpine. Conformance images correctly use `/health/ready` (readiness) and gate dependents via `condition: service_healthy`; the liveness-vs-readiness split is correct.
- Cross-arch is intentional and correct (`--platform=$BUILDPLATFORM` + `TARGETOS/TARGETARCH`; goreleaser `dockers_v2` multi-arch matches `${TARGETPLATFORM}/dfs` layout; operator buildx via `Dockerfile.cross`).
- goreleaser builds static + version-injected (`ldflags -s -w -X main.version/commit/date`), `goreleaser-action` SHA-pinned + `~> v2.15`.
- `/data` subdir creation + chown present in all three runtime images.

**Runtime / PID-1**
- dfs runs as PID 1 (exec-form ENTRYPOINT, no shell wrapper); `start.go:260-261` installs SIGINT/SIGTERM → cancels root ctx → waits on `serverDone`. Shutdown chain is well-ordered (snapshot-drain → stop adapters → flush metadata → close stores → stop API), each bounded by `shutdownTimeout`.
- Operator manager handles SIGTERM via `ctrl.SetupSignalHandler()` on distroless exec-form `["/manager"]`.
- EXPOSE ports match defaults in primary + release images (NFS 12049, SMB 12445, REST 8080); the 9090 drift is isolated to the non-release `Dockerfile.prebuilt`.
- No zombie-reaping/tini concern in foreground mode — the serve path spawns no children (gokrb5 is in-process, not a `kinit` subprocess).

**Test-parity / harness**
- `kdc-keytabs` shared-volume startup race handled via `klist -k` healthcheck + `depends_on: condition: service_healthy`.
- `linux/amd64` platform pins present on all reference images needing them (wpts, smbtorture, smbtorture-kerberos) for Apple Silicon.
- The three sibling KDC/nfs-client Dockerfiles use the correct hard-fail apt-retry idiom.
- KDC entrypoints (smb/nfs) pipe the master password via stdin to avoid `/proc/cmdline` exposure.
- `bootstrap.sh` is mounted read-only but invoked out-of-band via `docker exec`, not the ENTRYPOINT — intentional, not a missing-init bug.
- `docker-compose.override.yml` only remaps host `18080→8080` and is local-only.

---

## 7. Recommended PR-B shape

Split the one HIGH into a focused fix PR; file the rest as tracked issues per the project's MED/LOW-defer convention.

- **PR-B1 (HIGH H1) — operator shutdown-grace coupling.** In `dittoserver_controller.go`, set `PodSpec.TerminationGracePeriodSeconds` **derived from** the rendered `shutdown_timeout` (`preStop + shutdown_timeout + margin` minimum; `preStop + 3×shutdown_timeout` for the serial worst case). Add a CRD field (`api/v1alpha1/dittoserver_types.go`) so operators can tune both grace and `shutdown_timeout` together. Add a controller test asserting `TGPS > preStop + shutdown_timeout`. **This is the only v1.0-blocking item.** (Bundle L13 preStop-`/bin/sh` coupling here since it touches the same hook.)

- **Issue cluster — supply-chain (M1/M2/M3).** One tracker: add image SBOM (syft) + cosign image-signature + provenance for both server and operator (grant operator job `id-token: write`), pin all 7 bases by digest with Renovate, and `git rm --cached` the 57 MB `manager-linux-amd64`. Highest-leverage hardening; document `cosign verify` in the release footer.

- **Issue — build hygiene (M4 + L3 + L4 + L8).** Align operator builder on `golang:1.25` (kills the toolchain network-download + the server↔operator drift), drop `go build -a`, and `git rm`/`.gitignore` the generated `Dockerfile.cross`.

- **Issue — test-harness robustness (M5 + L10 + L11).** Fix the kerberos-image apt-retry idiom; pin `golang:alpine` and `pjdfstest`.

- **Issue — image-surface cleanup (L1 + L7 + L9 + L2).** Decide distroless-vs-Alpine for the server image (and drop tzdata if unused); delete orphaned root `Dockerfile.prebuilt` (removing the stray 9090 + uncommitted-binary reference); tighten root `.dockerignore`.

- **Issue — container config-contract (L5 + L6 + L14).** Coordinate with cross-area #9 (config surface): support `DITTOFS_*`-env-only boot of the stock image, document a writable `/tmp` emptyDir for read-only-rootfs, and guard the PID-1 daemon-fork footgun.

Defer all LOW parity/cosmetic items (L12, L15) to the cleanup issue or close as won't-fix-test-only.

---

## 8. Coverage

**Audited**: all four production server images (root `Dockerfile`, `Dockerfile.goreleaser`, `Dockerfile.prebuilt`, `.dockerignore`) and all four operator artifacts (`Dockerfile{,.cross,.prebuilt,.dockerignore}`); `.goreleaser.yml` + `.github/workflows/release.yml` (signing/SBOM/provenance/permissions); the operator controller-rendered dfs PodSpec/StatefulSet + CRD (`dittoserver_controller.go`, `api/v1alpha1/dittoserver_types.go`, `internal/controller/config/config.go`); the dfs PID-1/signal/graceful-shutdown chain (`start.go`, `runtime/lifecycle/service.go`, `pkg/config/defaults.go`, `runtime.go`); image-relevant app behavior (healthcheck route registration, `:8080` bind, GC/temp fallback paths, config `MustLoad`); and the test harness (`test/{nfs,smb}-conformance/*` Dockerfiles + compose + entrypoints + run.sh, `test/posix/*`, `test/integration/{kerberos,portmap}/*`).

**Cross-boundary seams explicitly checked** (round-2 charter): operator→server shutdown-grace contract (H1, the headline cross-area find), operator-image→dfs-image base coupling (L13), env-vs-file config contract (L6, flagged to cross-area #9), conformance-image↔production-image user/port parity, and EXPOSE-vs-actual-bind drift.

**Re-verified round-1**: the `11-operator` LOWs (committed binary L-SIMP-1, generated `Dockerfile.cross` L-SIMP-4) still reproduce on current develop and are carried forward (M3, L8). Round-1 did **not** assess image signing, SBOM, digest-pinning, or the shutdown-grace boundary — those gaps (M1, M2, H1) are this pass's net-new contribution. No shipped PR has closed any docker finding since round-1.

**Not audited / out of scope**: runtime Kubernetes behavior under a live cluster (PodDisruptionBudgets, actual node-drain timing, HPA interplay) — H1 was confirmed by static analysis of the rendered spec, not a live kill test; the operator's RBAC/webhook/leader-election surface (owned by area #11-operator); Helm chart values beyond the manager Deployment's TGPS; runtime registry/pull-policy and image-scanning CI (none present — implied by M1). No protocol-level (NFS/SMB wire) concerns surfaced in the docker layer, so the Samba/Linux cross-checks yielded no docker findings.
