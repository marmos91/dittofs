# v1.0 Security Verification Report (Wave 2, #1010)

**Date:** 2026-06-02
**Method:** Doc-only verification pass. Walked every Wave-1 area `REVIEW.md` / `REVIEW2.md`,
extracted the security-relevant HIGHs marked shipped, and confirmed each mitigation is present
and correct in the implemented code on `develop` (`HEAD = 3e37a2ad`). No source was modified.
Build sanity: `go build ./...` clean.

**Note on paths:** The Wave-1 REVIEWs cite `pkg/adapter/nfs/...`; that tree was since moved to
`internal/adapter/nfs/...` (NLM/portmap/grace remain under `pkg/adapter/nfs/`). File:line evidence
below reflects the current `develop` layout.

---

## Summary

| # | Item | Area | Result |
|---|------|------|--------|
| 1 | RPCSEC_GSS DATA call-header MIC verification (krb5 auth bypass) | NFS #4 H1 | **VERIFIED** |
| 2 | GSS per-call service-downgrade enforcement | NFS #4 C-MED | **VERIFIED** |
| 3 | `share_deny` enforced across open-owners (`NFS4ERR_SHARE_DENIED`) | NFS #4 H2 | **VERIFIED** |
| 4 | All-ones (READ-bypass) special stateid rejected on WRITE/SETATTR/LOCK | NFS #4 H3 | **VERIFIED** |
| 5 | WRITE enforces `SHARE_ACCESS_WRITE` | NFS #4 H3 | **VERIFIED** |
| 6 | SETATTR validates stateid + write-access on size change | NFS #4 H18 | **VERIFIED** |
| 7 | READ enforces `SHARE_ACCESS_READ` | NFS #4 H19 | **VERIFIED** |
| 8 | NLM crashed-client lock release (no longer a no-op) + grace gating | NFS #4 H14 | **VERIFIED** |
| 9 | NSM SM_NOTIFY sender authentication (monitored-list + source-addr) | NFS #4 H16 | **VERIFIED** |
| 10 | NSM SM_NOTIFY state-number monotonicity (replay defence) | NFS #4 H17 | **VERIFIED** |
| 11 | NLM/NFSv4 grace-period enforcement wired into production | NFS #4 H7 / lock #5 H-1 | **VERIFIED** |
| 12 | config-show secret masking (admin pw-hash, JWT secret, postgres pw) | config #9 | **VERIFIED** |
| 13 | Lock persistence seam wired in production (`SetLockStore`) | lock #5 H-2 | **VERIFIED** |
| 14 | Cross-store byte-range conflict detection (SMB vs NLM/NFSv4) | lock #5 H-3 | **VERIFIED** |
| 15 | AllSquash/RootSquash applied per-call (v3 + v4) | auth squashing | **VERIFIED** |
| 16 | Operator: RFC 7807 error-contract decode (reconcile convergence) | operator #11 H1 | **VERIFIED** |
| 17 | Operator: Helm chart `networkpolicies` RBAC grant | operator #11 H2 | **VERIFIED** |
| 18 | Operator: finalizer-removal stale-RV 409 fix (`retryOnConflict`) | operator #11 R2 NEW-H1 | **VERIFIED** |
| 19 | Operator: managed pod `AutomountServiceAccountToken: false` | operator #11 R2 M-SEC-2 | **VERIFIED** |
| G1 | NFSv4 squash fail-OPEN on mapping error (v3 fails closed, v4 doesn't) | NFS #4 SF-4 | **GAP (MED)** |
| G2 | Operator → server credentials sent over plaintext HTTP | operator #11 M-SEC-1 | **GAP (MED)** |

**Tally: 19 VERIFIED, 2 GAPS (both MED). No HIGH/CRITICAL gaps found.**

All HIGH security mitigations called out across the Wave-1 audits are present and correct on
`develop`. The two residual gaps are MED defense-in-depth items already noted in the area REVIEWs;
they belong on the #1014 MED/LOW umbrella (no dedicated issue required).

---

## VERIFIED items (evidence)

### 1. RPCSEC_GSS DATA call-header MIC verification — NFS #4 H1
The DATA path now verifies the call-header MIC before doing anything else.
`internal/adapter/nfs/rpc/gss/framework.go:608` calls `verifyHeaderMIC(gssCtx.SessionKey,
headerPreimage, verifBody)` and returns `RPCSEC_GSS_CREDPROBLEM` on failure.
`verifyHeaderMIC` (`framework.go:746`) rejects a missing preimage, missing verifier, or empty
session key, unmarshals the RFC 4121 MIC token over the marshalled call header, and verifies with
`micToken.Verify(sessionKey, KeyUsageInitiatorSign)` (constant-time HMAC). The `Process` signature
threads the raw `headerPreimage` through (`framework.go:397`). The forged-handle / svc_none auth
bypass described in the audit is closed.

### 2. GSS per-call service-downgrade enforcement — NFS #4 C-MED
`framework.go:628`: `if cred.Service < gssCtx.Service` rejects with `RPCSEC_GSS_CREDPROBLEM`
("service downgrade … not permitted"). A context established at krb5p/krb5i can no longer be
downgraded to svc_none per call.

### 3. `share_deny` enforced across open-owners — NFS #4 H2
`internal/adapter/nfs/v4/state/manager.go:1139` calls `shareConflictLocked(...)` under `sm.mu` and
returns `ErrShareDenied` on conflict. `shareConflictLocked` (`manager.go:1226`) scans
`openStateByOther` for opens by a *different* owner on the same file and flags
`reqAccess&os.ShareDeny != 0 || reqDeny&os.ShareAccess != 0`. OPEN also validates
`share_access`/`share_deny` at decode (`v4/handlers/open.go:81-94`). `NFS4ERR_SHARE_DENIED` is now a
live code path, not a dead constant.

### 4–7. Stateid validation on READ/WRITE/SETATTR — NFS #4 H3/H18/H19
- `ValidateStateid` (`v4/state/stateid.go:151`): the all-ones READ-bypass stateid is accepted only
  when `op == StateidOpRead`; on any write-family op it returns `ErrBadStateid`
  (`NFS4ERR_BAD_STATEID`). The all-zeros anonymous stateid is allowed on READ and WRITE only.
- WRITE (`v4/handlers/write.go:132,148`): validates with `StateidOpWrite`, then enforces
  `openState.ShareAccess & OPEN4_SHARE_ACCESS_WRITE` → `NFS4ERR_OPENMODE`.
- SETATTR (`v4/handlers/setattr.go:99,117`): now calls `ValidateStateid` (the prior `_ = stateid`
  drop is gone) and enforces write access when the SETATTR changes size.
- READ (`v4/handlers/read.go:74,92`): captures `openState` and enforces
  `OPEN4_SHARE_ACCESS_READ` → `NFS4ERR_OPENMODE`.

### 8. NLM crashed-client lock release + grace gating — NFS #4 H14
`pkg/adapter/nfs/nlm.go:496` `handleClientCrash` → `releaseCrashedClientLocks` (`nlm.go:511`) now
does real work: iterates all shares, calls `lockMgr.ReleaseByOwnerPrefix(clientPrefix)`
(`nlm.go:556`), and drains queued waiters via `blockingQueue.RemoveClientWaiters` (`nlm.go:572`).
Defense-in-depth: it skips release while the share `IsInGracePeriod()` (`nlm.go:549`) so a
reconnecting client can still reclaim. The prior hard-coded `totalReleased = 0` stub is gone.

### 9–10. NSM SM_NOTIFY authentication + replay defence — NFS #4 H16/H17
`internal/adapter/nfs/nsm/handlers/notify.go:38` `Notify` gates every notification behind two checks
before any side effect:
- Gate 1 (`notify.go:61`) `isMonitoredFromSource(mon_name, ClientAddr)` (`notify.go:208`): the
  `mon_name` must match an SM_MON registration AND the RPC source IP must match that registration's
  recorded address — the classic statd-spoofing primitive is blocked.
- Gate 2 (`notify.go:72`) `admitPeerState(mon_name, state)` (`handlers/handler.go:209`): incoming
  state must strictly exceed the last-seen state for that `mon_name` (mutex-guarded `peerState`
  map); replays/stale notifications are dropped.
A failed gate drops the NOTIFY silently with no lock release.

### 11. Grace-period enforcement wired in production — NFS #4 H7 / lock #5 H-1
`pkg/metadata/service.go:503` constructs the manager via `lock.NewManagerWithGracePeriod(gpm)` and
`service.go:228` calls `lm.EnterGracePeriod(expectedClients)` at store open. The NLM admission path
gates on `lockMgr.IsOperationAllowed(...)` returning `ErrGracePeriod` → `NLM4_DENIED_GRACE_PERIOD`
(`pkg/adapter/nfs/nlm.go:94-103`). Grace is no longer a never-constructed no-op contract.

### 12. config-show secret masking — config #9 (#941/#973)
Three secret-bearing structs redact via `MarshalYAML`/`MarshalJSON` to the `"********"` sentinel:
- Admin password hash — `pkg/config/config.go:237-252`
- JWT signing secret — `pkg/controlplane/api/config.go:92-104`
- Postgres password — `pkg/controlplane/store/gorm.go:64-79`

`dfs config show` (`cmd/dfs/commands/config/show.go:69-84`) serializes YAML directly and produces
JSON by round-tripping through YAML (`yamlKeyedView`), so both output formats apply the redaction
hooks. An empty secret stays empty so "unset" remains distinguishable from "redacted." No plaintext
secret is emitted by config-show.

### 13. Lock persistence seam wired in production — lock #5 H-2
`pkg/metadata/service.go:216` calls `lm.SetLockStore(ls)` with the backend store — the previously
dead seam now has a production caller. With the store set, the lease-break / reclaim / cross-protocol
`lm.lockStore != nil` branches become live. (Memory backend remains volatile by design; badger/
postgres persist.)

### 14. Cross-store byte-range conflict detection — lock #5 H-3
The two byte-range maps are now cross-checked bidirectionally:
- `Manager.Lock` scans `lm.unifiedLocks[handleKey]` via `fileLockConflictsWithUnified`
  (`pkg/metadata/lock/manager.go:807-808`).
- `Manager.AddUnifiedLock` scans `lm.locks[handleKey]` via the same helper (verified in the
  function body). The helper (`manager.go:620`) returns a "cross-protocol byte-range conflict".
A conflicting SMB2 BRL and an NLM/NFSv4 BRL on the same range can no longer both be granted
(MS-FSA §2.1.5).

### 15. AllSquash / RootSquash applied per-call — auth squashing
`ApplyIdentityMapping` (`pkg/metadata/auth_identity.go:254`) implements all_squash
(`MapAllToAnonymous`) and root_squash (`MapPrivilegedToAnonymous`, UID 0 → anonymous).
- v3: all mutating handlers obtain the auth context via `GetCachedAuthContext`
  (`internal/adapter/nfs/v3/handlers/doc.go:44`) which wraps `BuildAuthContextWithMapping` →
  `ApplyIdentityMapping`. Verified WRITE (`write.go:214`), and the non-cached helpers
  (LOOKUP/ACCESS/COMMIT/LINK/READLINK/READDIRPLUS) call `BuildAuthContextWithMapping` directly. No
  mutating v3 handler bypasses squashing — refutes the C-HIGH AUTH_SYS squash-bypass fear.
- v4: `helpers.go:60` applies `ApplyIdentityMapping` per OPEN/op.
Export-level access (read-only, etc.) is checked at the same layer.

### 16. Operator RFC 7807 error-contract decode — operator #11 H1
`k8s/dittofs-operator/internal/controller/dittofs_client.go:90-99` now decodes the real problem+json
shape (`title`/`detail`/`status`/`code`), stamps `StatusCode` on `DittoFSAPIError`, and always
returns a typed error for `>=400`. `IsConflict`/`IsAuthError`/`IsNotFound` (`dittofs_client.go:141-152`)
are keyed on the HTTP status (409 / 401|403 / 404), not the dead `Code` field. The
operator-service-account 409 "already exists" branch is now reachable → reconcile converges.

### 17. Operator Helm chart `networkpolicies` RBAC — operator #11 H2
`k8s/dittofs-operator/chart/templates/manager-rbac.yaml:91-100` now grants
`networking.k8s.io/networkpolicies` with `create/delete/get/list/patch/update/watch`, matching
`config/rbac/role.yaml:82`. Chart-deployed operators can create the baseline + adapter
NetworkPolicies, so the CR reaches Ready and the network-isolation control is actually applied.

### 18. Operator finalizer-removal stale-RV fix — operator #11 REVIEW2 NEW-H1
Finalizer removal is wrapped in `retryOnConflict` (`dittoserver_controller.go:524`) which re-Gets the
object (`current`) immediately before `controllerutil.RemoveFinalizer(current, ...)` at line 533, so
the stale-resourceVersion 409 loop (CR stuck Terminating) is resolved.

### 19. Operator managed-pod SA token automount disabled — operator #11 REVIEW2 M-SEC-2
`dittoserver_controller.go:1004` sets `AutomountServiceAccountToken: ptr.To(false)` on the managed
`dfs` PodSpec; the network-exposed container no longer auto-mounts an unused namespace-`default` SA
API token.

### Generic security pass (no findings)
- **Hardcoded secrets:** none. A repo-wide scan for inlined credential literals in `cmd/`, `internal/`,
  `pkg/` returned nothing; all secrets come from env (`BindEnv`) or `SecretKeyRef`.
- **Control-plane auth:** `pkg/controlplane/api/router.go` puts only health/ready, `/api/v1/grace`,
  and `/auth/login` + `/auth/refresh` on public routes (intentional, like K8s probes); every other
  route is behind `JWTAuth` and admin operations behind `RequireAdmin`. No unauthenticated mutating
  endpoint.
- **Injection:** all `exec.Command` call sites (mount/unmount/editor/daemon helpers) use the
  arg-slice form — no shell-string interpolation, so no shell injection from user-supplied
  mountpoints/sources. Database access is via gorm (parameterized); no `fmt.Sprintf`-built SQL on the
  request path.

---

## GAPS (tracked follow-ups — #1014 MED/LOW umbrella)

### G1 — NFSv4 identity-mapping failure falls back to UNMAPPED identity (fail-open) — MED
**Evidence:** `internal/adapter/nfs/v4/handlers/helpers.go:60-67`. When
`h.Registry.ApplyIdentityMapping(shareName, originalIdentity)` returns an error, v4 sets
`effectiveIdentity = originalIdentity` (the un-squashed identity) and proceeds. v3 in the same
situation **fails closed**: `internal/adapter/nfs/v3/handlers/auth_helper.go:96-99` returns the error
and aborts the op.
**Why it matters:** a divergence in fail-mode between the two protocols. The error path is narrow
(the comment notes "share may not exist yet," in which case the op would fail downstream on share
lookup), so this is not a demonstrated live root/all-squash bypass — but a mapping error that is *not*
a missing-share (e.g. a transient store error while resolving the mapping) would let v4 act on the
client's raw UID, including UID 0, instead of the squashed anonymous identity. This is exactly the
SF-4 "security divergence" the NFS area-4 REVIEW flagged.
**Recommendation:** make v4 fail closed like v3 — return `NFS4ERR_*` (or a nobody/anonymous identity)
on mapping error rather than reusing the unmapped identity. Low-risk change, isolated to `helpers.go`.

### G2 — Operator → `dfs` API credentials sent over plaintext HTTP — MED
**Evidence:** `k8s/dittofs-operator/api/v1alpha1/helpers.go` `GetAPIServiceURL` hardcodes `http://`;
consumed by `auth_reconciler.go` Login/CreateUser. The bootstrap admin password and the operator
service-account password (grants the `operator` role) traverse the pod network in cleartext; bearer
tokens for RefreshToken/DeleteUser are likewise in clear. The operator-managed NetworkPolicies
restrict client ingress but do not protect operator→server traffic from in-cluster sniffing.
**Why it matters:** in-cluster credential disclosure to a co-tenant with packet-capture on the pod
network. Bounded by network isolation but a real exposure for the privileged operator account.
**Recommendation:** serve the control-plane API over TLS and emit a configurable `https://` scheme,
or document the exposure and gate behind a service-mesh/mTLS requirement. (As flagged M-SEC-1 in the
operator REVIEW.)

---

## Notes for the orchestrator
- **No HIGH/CRITICAL security gap** requires a dedicated issue. Both gaps are MED defense-in-depth and
  were already documented in the Wave-1 REVIEWs (NFS #4 SF-4, operator #11 M-SEC-1); fold into #1014.
- The operator `REVIEW2.md` claim that the two round-1 HIGHs "STILL REPRODUCE on develop" is **stale**
  — both (H1 RFC 7807, H2 chart RBAC) plus the REVIEW2 NEW-H1 finalizer bug and M-SEC-2 SA-token
  automount have since been fixed on `develop` and were re-verified here.
