# Phase 68: Protocol Correctness and Hot-Reload - Context

**Gathered:** 2026-03-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Two fixes: (1) NTLM negotiation advertises only implemented capabilities — remove encryption flags for unimplemented NTLM sealing, and (2) shares created at runtime via dfsctl are immediately visible to all protocol adapters (NFS mount, SMB tree connect, REST API) without server restart. Also remove stale TODO about NTLM encryption since SMB3 transport encryption is the chosen confidentiality path.

</domain>

<decisions>
## Implementation Decisions

### NTLM Flag Cleanup
- Remove `Flag128` (0x20000000) and `Flag56` (0x80000000) from `BuildChallenge` in `internal/adapter/smb/auth/ntlm.go`
- Do NOT add `FlagSeal` — NTLM-level sealing (RC4) will never be implemented
- Remove the TODO comment at line 356 about implementing NTLM encryption — SMB3 AES transport encryption (implemented in PR #285) is the only confidentiality path
- Keep static server flags — do NOT intersect with client Type 1 negotiate flags (matches Samba/Windows Server behavior)
- Keep `BuildChallenge()` signature as-is (no parameters) — no need to accept client flags
- Leave Version field at offset 48 as zero (optional, not required by Windows clients)
- If a client sends `FlagSeal` in Type 1, silently ignore it — don't set it in Type 2, client falls back to SMB3 transport encryption

### Hot-Reload Verification
- The `OnShareChange` callback infrastructure already exists (both NFS and SMB adapters subscribe)
- NFS adapter rebuilds pseudo-filesystem on share change; SMB adapter invalidates share cache
- API handler already calls `runtime.AddShare` which triggers `notifyShareChange()`
- Add E2E test regardless of whether existing wiring works — runtime share creation is a critical path that needs regression protection
- Test the full lifecycle: create share -> mount -> verify -> delete share -> verify mount fails
- Test across all three interfaces: NFS mount, SMB tree connect, REST API enumeration

### Claude's Discretion
- Whether to refactor any hot-reload wiring if gaps are found during implementation
- Exact E2E test structure and helper placement
- Whether to test share update (edit) in addition to create/delete

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### NTLM Protocol
- `internal/adapter/smb/auth/ntlm.go` — BuildChallenge function (line 339-439), flag definitions (line 117-196)
- MS-NLMP Section 2.2.2.5 — NegotiateFlags definition
- MS-NLMP Section 3.2.5.1.2 — Server challenge construction

### Share Hot-Reload
- `pkg/controlplane/runtime/shares/service.go` — OnShareChange callback (line 675-704), notifyShareChange (line 688-704)
- `pkg/adapter/nfs/adapter.go` — SetRuntime with OnShareChange subscriptions (line 356-439)
- `pkg/adapter/smb/adapter.go` — SetRuntime with OnShareChange subscriptions (line 170-208)
- `internal/adapter/smb/v2/handlers/handler.go` — RegisterShareChangeCallback and share cache (line 960-1009)
- `internal/adapter/smb/v2/handlers/tree_connect.go` — TreeConnect handler
- `internal/controlplane/api/handlers/shares.go` — CreateShare handler calling runtime.AddShare (line 298)

### Related Issues
- GitHub #215 — NTLM encryption flags
- GitHub #235 — Share hot-reload
- PR #285 — SMB3 encryption implemented (relates to #215)

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `OnShareChange` callback pattern: Already wired in both NFS and SMB adapters, triggers on AddShare/RemoveShare
- `invalidateShareCache()` in SMB handler: Cache invalidation for pipe share enumeration
- `pseudoFS.Rebuild(shares)` in NFS adapter: Pseudo-filesystem reconstruction on share change
- `registerBreakCallbacks`: Deduplicating lock manager registration pattern (used by both adapters)
- E2E test helpers: `test/e2e/helpers/shares.go` for share creation, `test/e2e/helpers/controlplane.go` for runtime setup

### Established Patterns
- Adapter initialization via `SetRuntime()` with OnShareChange subscription before initial share loop
- Share cache with mutex-protected invalidation (SMB handler)
- Static NTLM flag set in BuildChallenge (no client flag intersection)

### Integration Points
- `runtime.AddShare()` -> `shares.Service.AddShare()` -> `notifyShareChange()` -> all registered callbacks
- `runtime.RemoveShare()` -> same callback chain
- NFS pseudo-fs rebuild -> affects EXPORT listing and MOUNT resolution
- SMB share cache invalidation -> affects TREE_CONNECT and pipe share enumeration

</code_context>

<specifics>
## Specific Ideas

- PR #285 already implemented SMB3 transport encryption, making NTLM sealing unnecessary — this confirms the "remove flags" approach is correct
- User wants maximum Windows compatibility — static server flags (Samba pattern) is the right model
- E2E test should cover create + delete lifecycle across NFS + SMB + API for complete regression protection

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 68-protocol-correctness-and-hot-reload*
*Context gathered: 2026-03-20*
