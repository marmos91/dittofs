# NFS RPC/GSS/auth — findings

Scope: `internal/adapter/nfs/rpc/` + `rpc/gss/` + `nfs/auth/` + `nfs/middleware/` + top-level dispatch (`internal/adapter/nfs/{dispatch.go,connection.go}`) + `pkg/adapter/nfs/` (`{connection.go,dispatch.go,adapter.go}`).
Cross-checked vs RFC 5531 (ONC RPC), RFC 2203 (RPCSEC_GSS), RFC 2623 (AUTH_SYS over NFS), RFC 4121 (krb5 GSS), and Linux `net/sunrpc/auth_gss/svcauth_gss.c` / `fs/nfsd/`.

---

## HIGH

### [HIGH] RPCSEC_GSS call-header verifier (MIC) is NEVER verified — `internal/adapter/nfs/rpc/gss/framework.go:574` (handleData), `pkg/adapter/nfs/dispatch.go:53`
The RPC call verifier for an RPCSEC_GSS DATA request is, per RFC 2203 §5.3.3.2, a krb5 GSS MIC computed by the client over the RPC call header **through the credential** (the `rpc_gss_cred_t`). The server MUST `gss_verify_mic()` it (this is exactly what Linux `svcauth_gss_verify_header` / `gss_verify_data` does). Here the verifier body is plumbed in — `handleData(ctx, cred, verifBody, requestBody)` — but `verifBody` is **completely unused** in the function body. There is no call to any MIC-verify routine on the call path (grep for `.Verify(` shows only `integrity.go`/`privacy.go` body checks and reply-side computation).

Consequences:
- **For `krb5` (svc_none, `RPCGSSSvcNone`, framework.go:632): there is no cryptographic check at all on a DATA request.** The svc_none branch just sets `processedData = requestBody`. An attacker who learns a context handle (16 bytes, sent on the wire in cleartext on every DATA call) can forge arbitrary requests under the victim's principal — full authentication bypass for krb5. The sequence window (sequence.go) only stops *exact* replays; it does not authenticate the credential, the seq_num, the handle, or the args.
- For krb5i/krb5p the body MIC/Wrap is checked (integrity.go:68, privacy.go:200), which incidentally covers the args + an embedded seq_num, so the practical exposure is smaller — but the header verifier is still unverified, so the `gss_proc`/`service`/`handle` fields in the credential are unauthenticated and a krb5i/krb5p client gets no header-integrity guarantee per spec.

Fix: in `handleData`, before unwrapping, verify `verifBody` as a krb5 MIC over the marshalled call header up to and including the credential, using `gssCtx.SessionKey` with `KeyUsageInitiatorSign`. On failure return `AuthStatCredProblem` (RFC 2203 §5.3.3.3). This requires passing the raw header bytes (XID..cred) down — currently only `credBody`/`verifBody`/`requestBody` reach `Process`, so the header preimage must also be threaded from `handleRPCCall`.

### [HIGH] AUTH_SYS UID/GID accepted verbatim with no export squashing in the RPC/auth layer — `internal/adapter/nfs/middleware/auth.go:108`, `internal/adapter/nfs/auth/unix.go:96`
`ExtractHandlerContext` copies the client-supplied `unixAuth.UID/GID/GIDs` straight into the handler context with no transformation. AUTH_SYS is unauthenticated and trivially spoofable (RFC 2623 §2.3.1); a client can claim `uid=0`. Per CLAUDE.md rule 2, root/all-squash is supposed to be applied during mount in `CheckExportAccess`. This sub-area cannot confirm that the squash is actually enforced for every NFSv3 op (NFSv3 is stateless — there is no per-mount handle binding, so the credential on each individual RPC is what reaches the store). If `CheckExportAccess` squashing is not re-applied per-operation from the live credential (only at MOUNT time), then `root_squash`/`all_squash` are bypassable on NFSv3 by sending a forged UID on each op. **Action for the metadata/permissions sub-audit: verify squash is applied to the per-call AUTH_SYS credential, not just at mount.** Flagging HIGH here because the RPC layer hands raw uid=0 downstream with zero mediation and the synthetic-user path (unix.go:96) explicitly preserves raw creds for unknown UIDs including 0.

### [HIGH] No duplicate request cache (DRC) for non-idempotent NFSv3 ops — design gap (no implementation found)
Grep found no reply cache / XID dedup anywhere in the dispatch path (`dispatch.go`, `pkg/adapter/nfs/dispatch.go`, `connection.go`). NFSv3 over TCP still retransmits on client timeout; without a DRC, a retried non-idempotent op (CREATE-exclusive, REMOVE, RENAME, LINK, MKDIR, SETATTR-with-guard) can be re-executed, returning a spurious error (e.g. `EEXIST`/`ENOENT`) to the client even though the first attempt succeeded. Linux nfsd maintains the DRC (`fs/nfsd/nfscache.c`) precisely for this. Correctness, not pure DoS — marked HIGH because it produces silent client-visible data/operation errors. Fix: add an XID+checksum+source-addr keyed reply cache for non-idempotent procedures, or document the limitation explicitly.

### [HIGH] Default `MaxConnections = 0` (unlimited) — `pkg/adapter/nfs/adapter.go:236-246` (no default set), enforced only via live settings `adapter.go:492`
`MaxRequestsPerConnection` defaults to 100 and read/idle timeouts default to 5 min, but `MaxConnections` has **no default** (stays 0 = unlimited per the doc at adapter.go:162). `preAcceptCheck` only rejects when `liveSettings.MaxConnections > 0`. A remote attacker can open unlimited TCP connections; each spawns a `Serve` goroutine and can buffer up to `MaxFragmentSize` (1.25 MB) + 100 concurrent in-flight requests. Unbounded connections × 1.25 MB pooled buffers = trivial memory-exhaustion DoS. Fix: ship a sane non-zero default cap (e.g. 1024) and/or a global in-flight-bytes budget.

---

## MED

### [MED] `ReadData` reads attacker-controlled length fields from raw bytes with no bounds check → panic — `internal/adapter/nfs/rpc/parser.go:129-166`
`ReadData` independently re-parses the credential/verifier lengths from the raw message (`message[offset:offset+4]`) and advances `offset += int(credLen)` / `int(verfLen)` with **no check that `offset+4 <= len(message)`** before each slice, and no check that the advanced offset stays within bounds. If a message reaches `ReadData` whose embedded lengths are inconsistent with its actual size, this panics with slice-out-of-range. In the normal flow `ReadCall` (parser.go:62) fully XDR-unmarshals cred+verf first, so a malformed message fails earlier — but `ReadData` re-trusting the wire instead of using the already-decoded `call.Cred`/`call.Verf` lengths is fragile and a latent panic. Impact is contained to one request (recovered by `handleRequestPanic`, connection.go:283) so MED not HIGH. Fix: bounds-check every slice (`if offset+4 > len(message) { return error }`), or compute the offset from `len(call.Cred.Body)`/`len(call.Verf.Body)` which were already validated.

### [MED] Partial-read bug: `reader.Read()` used instead of `io.ReadFull()` in GSS decoders — `internal/adapter/nfs/rpc/gss/types.go:249` (handle), `rpc/gss/integrity.go:155` (readXDROpaque, also used by privacy.go)
`bytes.Reader.Read(data)` returns a *short* read (n < len(data), err == nil) when fewer bytes remain than requested, silently leaving the tail of the `make([]byte, length)` buffer as zeros. A truncated GSS handle or opaque body therefore decodes "successfully" into a partially-zeroed buffer instead of erroring. Compare `internal/adapter/nfs/auth/unix.go:146` which correctly uses `io.ReadFull`. Fix: replace `reader.Read(x)` with `io.ReadFull(reader, x)` in `DecodeGSSCred` (types.go:249) and `readXDROpaque` (integrity.go:155). Security-relevant for handle parsing (could enable handle confusion) so borderline HIGH; kept MED because the downstream context-lookup would fail on a wrong handle.

### [MED] Multi-fragment RPC records are not reassembled — `pkg/adapter/nfs/connection.go:187-223`, `internal/adapter/nfs/connection.go:40-52`
`readRequest` reads exactly one fragment header, validates its size, reads that one fragment, and parses it as a complete RPC message. The `IsLast` bit (parsed at internal/connection.go:49) is **never consulted** — there is no loop accumulating fragments until `IsLast==true`. RFC 5531 §11 permits a client to split one RPC record across multiple fragments. Linux clients normally send single fragments so interop is fine in practice, but a conformant client (or one that chunks large WRITEs) would have its first fragment misparsed as a whole message → GARBAGE_ARGS or worse. Also note: because each fragment is independently size-capped at 1.25 MB but there's no *aggregate* cap, if reassembly were added naively it would need a total-record bound. Fix: loop on `IsLast`, accumulate into one buffer with a hard aggregate cap, then parse.

### [MED] `extractAPReq` skips the krb5 mech OID without validating it — `internal/adapter/nfs/rpc/gss/framework.go:201`
The GSS-API initial-context-token parser reads the OID length and does `offset += oidLen` with the comment "we could verify it's the krb5 OID, but we trust the caller." Accepting a non-krb5 mech OID and then feeding the remainder to `extractAPReq`/`Authenticate` is sloppy; a mismatched/oversized `oidLen` is bounds-checked at framework.go:204 (`offset > len(token)`) so no overflow, but mechanism confusion should be rejected explicitly. Fix: compare the OID bytes against `KRB5OID` / `KerberosV5OIDBytes` and reject otherwise.

### [MED] GSS reply path falls back to AUTH_NULL verifier on MIC-compute failure — `pkg/adapter/nfs/dispatch.go:110-124`
On `ComputeInitVerifier` error the code logs at Debug and silently sends an AUTH_NULL verifier on a successful GSS INIT reply (gss_major=COMPLETE). A client doing mutual auth will reject the AUTH_NULL verifier, but masking a crypto failure behind a "success" reply is misleading and complicates diagnosis. Lower-impact (client-side reject), so MED. Fix: treat MIC-compute failure on a COMPLETE INIT as a hard error (return CTXPROBLEM) rather than degrading the verifier.

### [MED] Service-level downgrade between context and per-call is logged, not enforced — `internal/adapter/nfs/rpc/gss/framework.go:589-595`
`handleData` notes a `cred.Service != gssCtx.Service` mismatch with a `logger.Warn` but then proceeds to unwrap using `cred.Service` (the per-call value). RFC 2203 §5.3.3.4 does allow per-call service selection, but a context established as krb5p (privacy) that then accepts a svc_none DATA call is a confidentiality/integrity downgrade — combined with the HIGH "no header MIC" finding this means a sniffed krb5p handle could be reused at svc_none with zero crypto. Fix: pin/enforce the negotiated minimum service for the context (reject calls requesting a weaker service than the context/export requires).

---

## LOW

### [LOW] Two independent AUTH_UNIX parsers can drift — `internal/adapter/nfs/rpc/auth.go:117` vs `internal/adapter/nfs/auth/unix.go:123`
`rpc.ParseUnixAuth` and `auth.parseUnixAuth` are near-duplicates of the same RFC 1831 §9.2 wire format with the same caps (name ≤255, gids ≤16). The rpc-package version (auth.go:145) uses `binary.Read(reader, ..., &nameBytes)` for the name (works but unusual), the auth-package version uses `io.ReadFull`. Duplication invites divergence in bounds/validation. Fix: collapse to one parser (the auth pkg already comments that the duplication exists only to avoid an import cycle — resolve via a shared low-level package).

### [LOW] `ReadData` ignores its returned error contract — `internal/adapter/nfs/rpc/parser.go:109,166`
The doc says "Always returns nil (kept for interface compatibility)" yet the function is the one place that should surface a malformed-offset error (see MED above). The always-nil contract actively discourages adding the needed bounds check. Tighten the contract when fixing the bounds issue.

### [LOW] AUTH_SHORT / AUTH_DES advertised in constants but unsupported — `internal/adapter/nfs/rpc/auth.go:26-35`
`AuthShort`/`AuthDES` are defined and documented but there is no handling path; a client sending them falls through `ExtractHandlerContext` (middleware/auth.go:79 returns ctx with nil creds → effectively anonymous). Harmless (degrades to no-creds) but the unhandled flavors should be explicitly rejected rather than silently treated as unauthenticated. Note `AuthDES` comment calls it "also known as AUTH_DH" — AUTH_DES (3) and AUTH_DH are the same flavor, fine, but DES is deprecated and should not be advertised.

### [LOW] GSS context handle generated but store does not bind it to source connection/principal scoping — `internal/adapter/nfs/rpc/gss/context.go:229` (Lookup)
`ContextStore.Lookup` resolves a context purely by the 16-byte handle with no check that the requesting connection/peer matches the one that established it. Combined with the HIGH no-header-MIC finding this is what makes handle theft fully exploitable for krb5; once the MIC is enforced this becomes defense-in-depth only, hence LOW. Consider binding contexts to a client identifier.

---

## Notes on things that are CORRECT (checked, no finding)

- **Fragment size bound**: `ValidateFragmentSize` caps a single fragment at `MaxFragmentSize` (1.25 MB) before any allocation (`internal/connection.go:56`) — good, no unbounded per-fragment alloc.
- **AUTH_UNIX length caps**: machinename ≤255, gids ≤16 are enforced before allocation in both parsers (auth.go:139/177, unix.go:141/174) — RFC 1813 §2.5 compliant, no gid-array amplification.
- **GSS opaque caps**: `decodeOpaqueToken` ≤64 KB (framework.go:274), `DecodeGSSCred` handle ≤64 KB (types.go:242), `readXDROpaque` ≤1 MB (integrity.go:149) — all bounded before alloc.
- **Sequence window** (sequence.go): the modular bitmap correctly tracks `[highest-size+1, highest]`, rejects seq 0, seq > MAXSEQ, below-window, and duplicates; `slideWindow` clears exactly the re-entering slots. Mutex-guarded. Matches RFC 2203 §5.3.3.1 silent-discard semantics (framework.go:617 → SilentDiscard).
- **MAXSEQ handling**: `handleData` deletes the context and returns CTXPROBLEM on seq ≥ MAXSEQ (framework.go:604) — RFC 2203 compliant.
- **Context store ordering**: context is `Store()`d before the INIT reply is built/sent (framework.go:503) — avoids the documented Ganesha race.
- **krb5i/krb5p body crypto**: integrity.go and privacy.go do verify the MIC/Wrap over the body and dual-validate the embedded seq_num against the credential seq_num — correct (the gap is the *header* verifier, see HIGH).
- **Program/version dispatch**: PROG_UNAVAIL for unknown program (dispatch.go:201), PROG_MISMATCH with `[low,high]` range for bad version (dispatch.go:239 et al), PROC_UNAVAIL unmapped procedures fall to empty reply. Note: `pkg/adapter/nfs/dispatch.go:194` returns `RPCProcUnavail` for an unknown *program* (should be PROG_UNAVAIL) — minor inconsistency with `internal/.../dispatch.go:203` which correctly uses `RPCProgUnavail`; the internal Dispatch is the live path. (LOW-grade nit, folded here.)
- **Read deadline** covers both fragment-header and body read within one loop iteration (5-min default) — bounds slowloris-on-fragment for the single-fragment path.

---

## Severity tally
- HIGH: 4 (GSS header-MIC never verified [auth bypass for krb5]; AUTH_SYS raw uid passthrough / squash-enforcement-unconfirmed; no DRC for non-idempotent ops; unlimited default MaxConnections DoS)
- MED: 6 (ReadData unchecked length→panic; short-read in GSS decoders; no multi-fragment reassembly; OID not validated; AUTH_NULL fallback on INIT MIC failure; service-level downgrade not enforced)
- LOW: 5 (duplicate AUTH_UNIX parsers; ReadData always-nil error contract; unsupported flavors silently anonymous; handle not connection-scoped; PROG vs PROC_UNAVAIL nit)

Top finding: **GSS DATA call-header MIC is never verified (framework.go:574 / dispatch.go:53)** — for `krb5` (svc_none) this is a complete authentication bypass: a stolen/observed context handle lets an attacker forge requests under the victim principal with no cryptographic check.
