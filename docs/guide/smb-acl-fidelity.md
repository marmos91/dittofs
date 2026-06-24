# SMB Windows-ACL / Security-Descriptor Fidelity Matrix

This is the interop reference for what the DittoFS SMB Security Descriptor (SD)
path actually round-trips, derived from the code — not from the spec. It is the
"documented interop matrix" required by [#1228](https://github.com/marmos91/dittofs/issues/1228).

For the cross-protocol ACL *model* (why NFSv4 ACEs are the canonical form, the
evaluation algorithm, mode sync), see [Access Control](access-control.md). This document is
narrower: it states, aspect by aspect, whether the SMB SD wire path is
**Works**, **Partial**, or **Unsupported**, with the source of truth for each.

## Legend

- **Works** — round-trips faithfully through QUERY_INFO (read) → SET_INFO
  (write) → QUERY_INFO with no semantic loss.
- **Partial** — round-trips with a documented caveat or under conditions.
- **Unsupported** — dropped, stubbed, or not implemented; behavior noted.

Code paths referenced:

| Path | Function(s) | File |
|------|-------------|------|
| SD read (build) | `BuildSecurityDescriptor`, `buildDACL`, `buildSACL`, `buildEmptySACL` | `internal/adapter/smb/handlers/security.go` |
| Read-side DACL synthesis | `SynthesizeWindowsDefault`, `SynthesizeFromMode` | `pkg/metadata/acl/synthesize.go` |
| SD write (parse) | `ParseSecurityDescriptorWithOptions`, `parseDACL`, `parseACEs` (shared DACL/SACL) | `internal/adapter/smb/handlers/security.go` |
| SET_INFO Security | `setSecurityInfo`, `checkSetInfoSecurityAccess` | `internal/adapter/smb/handlers/set_info.go` |
| SID derivation | `UserSID`, `GroupSID`, `UIDFromSID`, `GIDFromSID`, `PrincipalToSID`, `SIDToPrincipal` | `pkg/auth/sid/mapper.go` |
| Generic-mask expansion | `ExpandGenericMask` | `pkg/metadata/acl/generic.go` |
| Inheritance | `ComputeInheritedACL`, `PropagateACL`, flag translation | `pkg/metadata/acl/inherit.go`, `pkg/metadata/acl/flags.go` |
| Storage | `FileAttr.ACL` / `SetAttrs.ACL` | `pkg/metadata/file_types.go` |

NFSv4 ACE mask bits and Windows `ACCESS_MASK` bits share identical positions by
RFC 7530 design, so masks need no translation. Only the principal format differs
(NFSv4 `who@domain` strings vs. binary SIDs) and the inheritance-flag bit for
`INHERITED_ACE` (NFSv4 `0x80` vs. Windows `0x10`, translated in `flags.go`).

## Owner / Group SID

| Aspect | State | Notes |
|--------|-------|-------|
| Owner SID read | Works | `BuildSecurityDescriptor` emits `mapper.UserSID(file.UID)`. UID 0 → `BUILTIN\Administrators` (`S-1-5-32-544`); all other UIDs → domain SID `S-1-5-21-{machine}-{uid*2+1000}`. |
| Group SID read | Works | `mapper.GroupSID(file.GID)` → `S-1-5-21-{machine}-{gid*2+1001}`. The `+1001` (vs `+1000`) offset guarantees `UserSID(n) != GroupSID(n)`. |
| Owner SID write | Works (round-trips local) | Parsed only when `OwnerSecurityInformation` is requested AND `UIDFromSID` recognizes the SID (machine-domain, even RID offset ≥ 1000). Recognized → `SetAttrs.UID`. Unrecognized owner SID is silently ignored (UID unchanged). |
| Group SID write | Works (round-trips local) | `GIDFromSID` (odd RID offset); falls back to `UIDFromSID` if the group SID actually encodes a user. Unrecognized → ignored. |
| Owner/Group section gating | Works | Write requires `WRITE_OWNER` on the open's `GrantedAccess` (`checkSetInfoSecurityAccess`); both sections fold under one `WRITE_OWNER` gate per MS-DTYP §2.5.3.3. |

Note: `OWNER@`/`GROUP@` *inside a DACL ACE* resolve to the file's current
owner/group SID via `principalToSID` (`UserSID`/`GroupSID`) — NOT to
`CREATOR_OWNER`/`CREATOR_GROUP`. The CREATOR placeholders (`S-1-3-0`/`S-1-3-1`)
are distinct and only meaningful as inheritable placeholders (see Inheritance).

## DACL — ACE count, type, mask

| Aspect | State | Notes |
|--------|-------|-------|
| ALLOW ACE (type 0x00) | Works | `accessAllowedACEType` ↔ `ACE4_ACCESS_ALLOWED_ACE_TYPE`, both directions. |
| DENY ACE (type 0x01) | Works | `accessDeniedACEType` ↔ `ACE4_ACCESS_DENIED_ACE_TYPE`. Stored and round-tripped; canonical ordering (deny before allow) is the ACL engine's concern. |
| Access mask bits | Works | Identical bit positions; written/parsed verbatim. Generic bits are expanded on write (see below). |
| ACE count | Partial | Capped at `acl.MaxACECount` (128). `parseDACL` rejects a DACL whose header AceCount exceeds the cap with an error → SET_INFO returns `STATUS_INVALID_PARAMETER`. DACL size cap is `acl.MaxDACLSize` (64 KiB). |
| Unknown ACE types on write | Partial | `parseDACL` skips ACE types it cannot map (anything other than ALLOW/DENY/AUDIT) — the ACE is dropped, the rest of the DACL still parses. |
| Empty DACL (0 ACEs) | Works | `file.ACL != nil, len(ACEs)==0` emits a 0-ACE DACL (deny-all) on read; no default is synthesized. |
| nil ACL on read | Partial (synthesized) | `file.ACL == nil` → `SynthesizeWindowsDefault`: ALLOW `OWNER@` + ALLOW `SYSTEM@`, both FullControl, no inherit flags, `Protected=true` (SD control `0x9004`). This is a *display* default; server-side access enforcement still uses POSIX mode bits for nil-ACL files. `SynthesizeFromMode` (POSIX-mode → Allow-only DACL) exists but is currently only exercised by tests. |

## Inheritance flags

| Flag | Wire | NFSv4 | State | Notes |
|------|------|-------|-------|-------|
| OBJECT_INHERIT (OI) | 0x01 | `ACE4_FILE_INHERIT_ACE` 0x01 | Works | Translated by `flags.go`. |
| CONTAINER_INHERIT (CI) | 0x02 | `ACE4_DIRECTORY_INHERIT_ACE` 0x02 | Works | |
| NO_PROPAGATE (NP) | 0x04 | `ACE4_NO_PROPAGATE_INHERIT_ACE` 0x04 | Works | Stops propagation at first child; honored in `ComputeInheritedACL`. |
| INHERIT_ONLY (IO) | 0x08 | `ACE4_INHERIT_ONLY_ACE` 0x08 | Works | IO ACEs skipped in evaluation, kept for children. |
| INHERITED_ACE | 0x10 | `ACE4_INHERITED_ACE` 0x80 | Works | Bit position differs (0x10 ↔ 0x80) and is correctly remapped — a naive truncation would corrupt this. |
| Inheritance computation at create | Works | — | Works | `ComputeInheritedACL` mirrors Samba `create_descriptor.c`; CREATOR_OWNER/CREATOR_GROUP (`S-1-3-0`/`S-1-3-1`) placeholders are substituted with the creator's frozen identity at create time. |
| Recursive propagation on parent change | Works | — | Works | `PropagateACL` recomputes inherited ACEs while preserving explicit ones and child per-SD flags. |

## SD control flags

| Flag | State | Notes |
|------|-------|-------|
| SE_SELF_RELATIVE (0x8000) | Works | Always set; all SDs are self-relative. |
| SE_DACL_PRESENT (0x0004) | Works | Set when `DACLSecurityInformation` requested. |
| SE_DACL_PROTECTED (0x1000) | Works | Read from `ACL.Protected`; written from inbound Control. Blocks inheritance from ancestors; never itself inherited onto children. |
| SE_DACL_AUTO_INHERITED (0x0400) | Works (canonicalized) | Round-trips via `ACL.AutoInherited`. Default canonicalization (MS-DTYP §2.5.3.4.2, mirrors Samba `canonicalize_inheritance_bits`): persisted only when SET_INFO carries BOTH `AUTO_INHERITED` and `AUTO_INHERIT_REQ` (0x0100). Per-share opt-out (`acl flag inherited canonicalization = no`) preserves it verbatim. |
| SE_DACL_AUTO_INHERIT_REQ (0x0100) | Works (request-only) | Processed as a request flag; never echoed back on read. |
| SE_SACL_PRESENT (0x0010) | Works | Set when `SACLSecurityInformation` requested; the SACL body carries stored audit ACEs (or a valid 0-ACE SACL when none are stored) — see SACL below. |

## Null DACL

| Aspect | State | Notes |
|--------|-------|-------|
| Read | Works | `ACL.NullDACL` → `SE_DACL_PRESENT` set with `daclOffset == 0` (no DACL body) = everyone-full-access semantics. |
| Write | Works | SD with `SE_DACL_PRESENT` and zero DACL offset → `&acl.ACL{NullDACL: true}`. Also produced when `DACLSecurityInformation` is requested but the SD carries no DACL (`setSecurityInfo`). |

## SACL (audit)

| Aspect | State | Notes |
|--------|-------|-------|
| SACL read | Works | When `SACLSecurityInformation` is requested and the file carries stored SACL ACEs, `buildSACL` serializes them into the SD's SACL section (same MS-DTYP §2.4.5 + §2.4.4.2 wire layout as a DACL). Windows Explorer's "Auditing" tab shows the stored audit ACEs. `buildEmptySACL` is the fallback only when no SACL is stored (valid 0-ACE SACL). |
| SACL write | Works | `ParseSecurityDescriptorWithOptions` parses the SACL body via the shared `parseACEs` and persists audit ACEs into `ACL.SACL`; `setSecurityInfo` installs it via `mergeSecurityACL`, preserving the unrequested DACL/SACL section. Access-gated (`AccessSystemSecurity` required). |
| AUDIT / ALARM ACE round-trip | Works | `ACE4_SYSTEM_AUDIT_ACE_TYPE` ↔ `systemAuditACEType` and `ACE4_SYSTEM_ALARM_ACE_TYPE` ↔ `systemAlarmACEType` map both directions (`nfsToWindowsACEType` / `windowsToNFSACEType`); the SUCCESSFUL/FAILED audit flags round-trip through `NFSv4FlagsToWindowsFlags`. Audit ACEs ride the SACL section, not the DACL. |

The SACL is stored on the same `acl.ACL` carrier as the DACL (`ACL.SACL`
slice) and never participates in access checks. The store-layer round-trip
shipped under [#1228](https://github.com/marmos91/dittofs/issues/1228); the SMB
wire parse/serialize under
[#1381](https://github.com/marmos91/dittofs/pull/1381).

## GENERIC_* mask expansion

| Aspect | State | Notes |
|--------|-------|-------|
| GENERIC_READ / WRITE / EXECUTE / ALL on write | Works | `parseDACL` calls `ExpandGenericMask` on each ACE before storing, per MS-DTYP §2.5.3 / MS-FSA §2.1.5.1.2.1. Generic bits are expanded to file-object-specific rights and the generic bits stripped (e.g. `GENERIC_ALL 0x10000000` → `FILE_ALL_ACCESS 0x001F01FF`). |
| Generic bits at inherit time | Works | `ComputeInheritedACL` expands generic bits on effective (non-INHERIT_ONLY) ACEs against the child object type; INHERIT_ONLY placeholders keep generic bits for the eventual leaf. |
| Generic bits on read | n/a | Stored masks are already specific (expanded on write), so read emits specific rights. |

## Foreign (AD/LDAP) SID mapping

| Aspect | State | Notes |
|--------|-------|-------|
| Foreign domain SID → local UID/GID (write) | Unsupported / out of scope | A SID outside the joined domain is not decodable to a local UID/GID. As an owner/group it is ignored; inside a DACL ACE it round-trips as the `sid:<canonical>` principal form (preserved verbatim by `SIDToPrincipal`/`PrincipalToSID`), so the ACE is kept but the SID maps to no local UID/GID. AD integration shipped under [#1231](https://github.com/marmos91/dittofs/issues/1231) (Kerberos/NTLM logon, NETLOGON machine credential, `idmap_rid` for the *joined* domain); mapping a *foreign* (non-joined) domain SID to a local UID/GID remains out of scope. Foreign-SID → **name** display does resolve — see below. |
| Named NFS principal → SID (e.g. `alice@EXAMPLE.COM`) | Partial (lossy) | `PrincipalToSID` synthesizes a hash-based fallback SID (full 32-bit hash as a 6th sub-authority) so it is NOT decodable back to a numeric UID and round-trips through `sid:`. Deterministic but not a real AD SID. |
| SID → name resolution (display) | Works | LSA `\pipe\lsarpc` is implemented (`internal/adapter/smb/rpc/lsarpc.go`, all 7 ops incl. `LsarLookupSids2/3`, `LsarOpenPolicy3`). Windows Explorer's Security tab resolves machine-domain user/group SIDs and well-known SIDs to names; foreign AD-domain SIDs resolve via the foreign resolver (`pkg/adapter/smb/lsarpc_foreign.go`). Shipped under [#236](https://github.com/marmos91/dittofs/issues/236) / [#1341](https://github.com/marmos91/dittofs/issues/1341). |

## Unmappable-SID-on-write behavior

| Scenario | Behavior |
|----------|----------|
| Owner/Group SID not recognized by `UIDFromSID`/`GIDFromSID` | Section silently ignored; UID/GID unchanged. No error returned. |
| DACL ACE SID not a local domain SID | `SIDToPrincipal` returns `sid:<canonical>`; ACE stored with that `Who`. Preserved on subsequent reads via `PrincipalToSID`'s `sid:` round-trip. |
| Well-known SIDs (Everyone `S-1-1-0`, SYSTEM `S-1-5-18`, BUILTIN\Administrators `S-1-5-32-544`, OWNER_RIGHTS `S-1-3-4`, CREATOR_OWNER `S-1-3-0`, CREATOR_GROUP `S-1-3-1`) | Mapped to/from their NFSv4 special principals (`EVERYONE@`, `SYSTEM@`, `ADMINISTRATORS@`, `OwnerRights@`, `CreatorOwner@`, `CreatorGroup@`). Works. |
| DACL AceCount > 128 or DACL > 64 KiB | `parseDACL` errors → SET_INFO returns `STATUS_INVALID_PARAMETER`. |

## Access gating on SET_INFO Security

`checkSetInfoSecurityAccess` authorizes each requested section against the
**open's** `GrantedAccess` (captured at CREATE, per MS-SMB2 §3.3.5.21.3), not
the file's current DACL:

| Requested section | Required right |
|-------------------|----------------|
| DACL (`0x04`) | `WRITE_DAC` |
| OWNER / GROUP (`0x01` / `0x02`) | `WRITE_OWNER` |
| SACL (`0x08`) | `ACCESS_SYSTEM_SECURITY` |

Any requested section lacking its bit denies the whole request with
`STATUS_ACCESS_DENIED`.

## Client-tested

Behaviors confirmed against real clients (per repo history, CLAUDE.md, and
project memory):

- **Windows 11 Explorer — Security tab.** SD read/build path verified through
  the Phase 31/32 Windows integration work: explicit-Deny synthesis was removed
  in favor of Allow-only DACLs (Samba convention) so Explorer no longer shows
  spurious "Write: Deny"; CREATE-context (MxAc) parsing and SET_INFO attribute
  persistence (Hidden/ReadOnly) were fixed. SID-to-name resolution is
  implemented (LSA `lsarpc` pipe, [#236](https://github.com/marmos91/dittofs/issues/236)),
  so Explorer resolves user/group and well-known SIDs to names; raw SIDs appear
  only for principals with no directory entry.
- **AD / Kerberos SMB.** `alice@REALM` → uid 10001 verified live against an
  AD-DC over Kerberos-bound SMB (idmap_rid SID→UID), confirming the
  domain-SID derivation in `mapper.go` end-to-end for the local-domain case.
- **smbtorture `smb2.acls`.** The SD build/parse, inheritance
  (INHERITANCE/INHERITFLAGS), GENERIC expansion, and AUTO_INHERITED
  canonicalization paths are exercised by the SMB conformance suite (see
  inline references in `security.go`, `inherit.go`, `generic.go`).
- **SACL round-trip.** Audit-ACE build → parse → re-build is covered by
  `security_sacl_test.go` (`TestBuildSD_SACL_RoundTrip`): SE_SACL_PRESENT, the
  serialized ACE count, and the audit ACE's type/flags/mask all survive a
  full SD round-trip.
- **macOS Finder.** Not separately validated for the ACL Security path; treat
  as untested for ACL fidelity.

## Known limitations

1. **SACL round-trips but does not drive audit-event generation.** Audit/alarm
   ACEs are parsed, stored, and surfaced to the Windows "Auditing" tab
   (read/write/round-trip via `ACL.SACL`), and the `ACCESS_SYSTEM_SECURITY` gate
   is enforced. What is *not* implemented is acting on them: the server emits no
   audit log when an operation matches a SUCCESSFUL/FAILED audit ACE, and ALARM
   ACEs raise no alarm. The SACL is descriptive metadata only — it never
   participates in access checks or event emission.
2. **Foreign AD/LDAP SIDs are not mapped to local UID/GID.** They round-trip
   as opaque `sid:<canonical>` principals (so the ACE survives) but resolve to
   no local UID/GID. AD interop shipped under [#1231](https://github.com/marmos91/dittofs/issues/1231)
   (logon + `idmap_rid` for the joined domain); foreign-SID → local-UID idmap
   specifically stays out of scope. Foreign-SID → **name** display does resolve
   via `lsarpc` (see limitation 4).
3. **Hash-based fallback SIDs for named principals are lossy.** Deterministic
   but not real AD SIDs; collisions are vanishingly rare but theoretically
   possible.
4. **LSA SID→name pipe is implemented** ([#236](https://github.com/marmos91/dittofs/issues/236),
   `internal/adapter/smb/rpc/lsarpc.go`). Explorer resolves machine-domain and
   well-known SIDs — and foreign AD-domain SIDs via the foreign resolver
   (`pkg/adapter/smb/lsarpc_foreign.go`) — to names. Raw SIDs appear only for
   principals with no directory entry.
5. **nil-ACL access enforcement uses POSIX mode, not the synthesized DACL.**
   The `SynthesizeWindowsDefault` DACL shown to SMB clients for nil-ACL files is
   display-only; server-side access checks fall back to Unix mode bits, which
   stay authoritative.
6. **Owner/Group live in `FileAttr`, not in the ACL.** Changing owner/group
   does not emit ACL-change events (see [Access Control](access-control.md#known-limitations)).

## References

- [#1228](https://github.com/marmos91/dittofs/issues/1228) — Windows-ACL / stable-handle fidelity (this matrix).
- [#1231](https://github.com/marmos91/dittofs/issues/1231) — AD/LDAP enterprise integration (shipped).
- [#236](https://github.com/marmos91/dittofs/issues/236) — SID-to-name (LSA) resolution (shipped).
- [Access Control](access-control.md) — cross-protocol ACL model and tradeoff analysis.
- [MS-DTYP §2.4.4–2.4.6](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) — SID / ACE / ACL / Security Descriptor formats.
- [RFC 7530 §6](https://www.rfc-editor.org/rfc/rfc7530#section-6) — NFSv4 ACL model.
