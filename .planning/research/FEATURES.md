# Feature Landscape: v0.10.0 Production Hardening + SMB Protocol Fixes

**Domain:** Production hardening (quotas, client tracking, trash), SMB protocol completeness (credits, multi-channel, conformance, macOS signing)
**Researched:** 2026-03-20
**Confidence:** HIGH for SMB protocol mechanics (MS-SMB2 spec verified), MEDIUM for implementation complexity (codebase analysis + existing infrastructure), MEDIUM for trash/soft-delete (vendor patterns, no RFC standard)

---

## Table Stakes

Features users expect from a production NFS/SMB server. Missing these means administrators cannot operate DittoFS in real deployments.

### 1. Share Quotas with FSSTAT/FSINFO/SMB Reporting

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Per-share quota configuration | Every production NAS enforces storage limits. Without quotas, one share consumes all available storage. Admins refuse to deploy without this. | Medium | Control plane store (ShareStore), dfsctl share commands | Add `quota_bytes` field to Share model. Enforce during WriteFile when cumulative size would exceed quota. |
| FSSTAT/FSINFO quota-aware reporting (NFS) | `df` on NFS clients must show quota-limited free space, not raw disk capacity. NetApp, Dell PowerStore, and all enterprise NAS vendors do this. | Low | Existing `fsstat.go` handler, `GetFilesystemStatistics` in metadata service | Currently returns hardcoded/system values. Must return `TotalBytes = min(quota, physical)` and `AvailableBytes = quota - used`. |
| FileFsSizeInformation / FileFsFullSizeInformation (SMB) | Windows Explorer, `dir`, and PowerShell `Get-PSDrive` show free space from these info classes. Currently uses fallback values (1000000 clusters / 500000 available). | Low | `query_info.go` case 3 and case 7 already call `GetFilesystemStatistics`. Need quota-aware values from metadata service. | When quotas are set, `TotalAllocationUnits` and `CallerAvailableAllocationUnits` must reflect quota limits. WPTS tests `FsInfo_Query_FileFsSizeInformation` and `FsInfo_Query_FileFsFullSizeInformation` verify consistency between the two info classes. |
| Quota enforcement on write | Writes that would exceed quota must return `NFS3ERR_NOSPC` (NFS) or `STATUS_DISK_FULL` (SMB). Silently allowing over-quota writes is unacceptable. | Medium | WriteFile path in metadata service, per-share BlockStore | Must check cumulative used space + write size <= quota atomically. Race condition risk if two concurrent writes both check before either commits. Use optimistic check + post-write verify. |
| `dfsctl share` quota management | Admins must be able to set, view, and modify quotas via CLI. | Low | Existing dfsctl share commands, REST API handlers | Add `--quota` flag to `share create` and `share update`. Show quota in `share list` output. |
| Soft vs hard quota | Soft quotas warn but allow writes; hard quotas reject. Most enterprise NAS supports both. | Low | Configuration model | Start with hard quotas only (simplest, safest). Soft quotas can be added later with a warning log/metric. |

**How production servers handle quotas:**
- Linux knfsd: Delegates to the host filesystem's quota subsystem (`rpc.rquotad` for NFS clients). FSSTAT reports quota-limited space when user quotas are active. Tree quotas reflected in df output for both NFS and CIFS.
- Windows Server: Per-share quotas via File Server Resource Manager (FSRM). Reported through FileFsFullSizeInformation automatically. Supports hard/soft/warning thresholds.
- Samba: Uses underlying filesystem quotas (XFS, ext4). `dfree command` hook to customize `df` reporting.
- ONTAP: Tree quotas automatically reflected in NFS `df` output. User quotas reflected only in CIFS mapped drive size.

**Edge cases:**
- Quota check during WRITE must account for sparse files (allocated vs logical size)
- Truncate to larger size should check quota even though no blocks are written yet
- Block deduplication means UsedSize != sum of file sizes (content-addressed storage)
- S3-backed shares: quota applies to local cache + remote, but reporting should use logical file sizes

### 2. Payload Stats (UsedSize)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Accurate UsedSize in BlockStore.Stats() | Currently `UsedSize` in `blockstore.Stats` is either zero or inaccurate. Quota enforcement and FSSTAT/FSINFO depend on knowing actual storage consumption. | Medium | `pkg/blockstore/engine/engine.go`, local store implementations | Must track cumulative block size in the local store. Content-addressed dedup means summing unique block sizes, not file sizes. |
| Per-share used size tracking | Each share needs independent used-size tracking for quota enforcement and reporting. | Medium | `shares.Service`, per-share BlockStore instances | Since each share has its own BlockStore, this naturally scopes to per-share. Need efficient incremental tracking (add on write, subtract on delete/GC). |
| Used size in metadata layer | Metadata stores should track sum of file sizes (logical) for FSSTAT/FSINFO reporting. Physical usage comes from BlockStore.Stats(). | Low | MetadataStore implementations (memory, badger, postgres) | Simple counter incremented/decremented on file size changes. |

**Expected behavior:**
- `df` on NFS mount shows: Used = sum of file sizes (logical), Available = quota - used
- Windows Explorer: Shows used/free matching FileFsFullSizeInformation
- `dfsctl share list`: Shows used/quota/available per share

### 3. SMB Credit Flow Control (Grant/Charge Accounting)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Credit charge validation on requests | MS-SMB2 3.3.5.2.3: Server MUST verify CreditCharge matches payload size. If `CreditCharge` is 0 and payload > 64KB, fail with `STATUS_INVALID_PARAMETER`. If calculated charge > CreditCharge, fail. | Medium | `session/credits.go` (exists), `dispatch.go` pre-handler validation | DittoFS already has `CalculateCreditCharge()` and adaptive grant logic. Missing: request-side validation that incoming CreditCharge is sufficient for the payload. |
| Sequence number window tracking | MS-SMB2 3.3.1.1: Server tracks valid message sequence numbers (MessageId). Each credit corresponds to a sequence number slot. Client must use sequence numbers within its granted window. | High | Connection state, dispatch pre-check | This is the core of correct credit implementation. Without it, clients can send out-of-order or replayed MessageIDs. Windows clients expect this. |
| Credit grant never reduces to zero | MS-SMB2 3.3.1.2: "The server MUST ensure that the number of credits held by the client is never reduced to zero." If this happens, the client cannot send any more requests -- deadlock. | Low | Already handled by `MinimumCreditGrant = 1` in `credits.go` | Verify the grant path always returns >= 1. Current adaptive algorithm has min grant = 16, which satisfies this. |
| Multi-credit I/O operations | READ/WRITE operations > 64KB require multiple credits. CreditCharge = ceil(payload / 65536). Already implemented in `CalculateCreditCharge`. | Low | Already implemented | Need to wire validation into dispatch. |
| Credit response in every reply | Every SMB2 response header includes `CreditResponse` field with grants. Already wired in `response.go`. | Low | Already implemented | Verify all response paths set CreditResponse correctly, including error responses. |

**How production servers handle credits:**
- Windows Server: Grants 256-512 initial credits. Uses vendor-specific adaptive algorithm. Tracks sequence number windows per connection. Validates CreditCharge on every request.
- Samba: Default 512 initial credits. `smb2 max credits = 8192`. Validates CreditCharge. Tracks sequence number bitmap.
- ONTAP: Default 128 credits. Configurable. Reported issues with stale credit grants causing client timeouts on high-latency connections.

**Critical edge cases:**
- CANCEL requests consume 0 credits (special case in MS-SMB2)
- Async responses (CHANGE_NOTIFY, oplock break) use interim responses that do not consume credits
- Compounded requests: credit charge applies to each request individually, but total charge is sum
- Session binding (multi-channel): credits are per-connection, not per-session

### 4. SMB 3.1.1 Signing on macOS (Preauth Integrity Hash Fix)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Fix preauth integrity hash computation | macOS SMB client sends SMB 3.1.1 NEGOTIATE but DittoFS derives wrong signing key due to hash mismatch. Results in STATUS_ACCESS_DENIED on TreeConnect because signature verification fails. | Medium | `crypto_state.go`, `hooks.go`, `kdf/kdf.go`, `session/crypto_state.go` | The preauth hash chain is: Hash(zeros + NegReq) -> Hash(prev + NegResp) -> Hash(prev + SessSetupReq1) -> Hash(prev + SessSetupResp1) -> ... -> Hash(prev + final SessSetupReq). The hash used for KDF is the value AFTER the final SESSION_SETUP request (NOT including the final response). A single byte mismatch anywhere in the chain produces wrong signing keys. |
| macOS SPNEGO token differences | Apple's SMB client may use different SPNEGO wrapping or NTLM token layout than Windows. The hash includes the full packet bytes, so any difference in packet construction matters. | Medium | Existing SPNEGO/NTLM auth code | Need packet capture from macOS client to compare exact byte sequences. May need to handle Apple-specific NTLM negotiation flags. |

**How the preauth hash chain works (verified from MS-SMB2 spec + Microsoft test vectors):**
1. `H0 = zeros(64)` (64 bytes of zeros)
2. `H1 = SHA-512(H0 || NegotiateRequest)` -- full packet including SMB2 header
3. `H2 = SHA-512(H1 || NegotiateResponse)` -- full packet including SMB2 header
4. After server assigns SessionId in NegotiateResponse, create per-session hash starting from `H2`
5. `H3 = SHA-512(H2 || SessionSetupRequest[1])` -- first leg
6. `H4 = SHA-512(H3 || SessionSetupResponse[1])` -- with STATUS_MORE_PROCESSING_REQUIRED
7. `H5 = SHA-512(H4 || SessionSetupRequest[2])` -- final leg (NTLM auth complete)
8. `SigningKey = KDF(SessionKey, "SMBSigningKey\0", H5)`

**Common pitfalls (from Microsoft blog):**
- Hash must include complete SMB2 header (64 bytes) + body, NOT just the body
- Signature field in SESSION_SETUP responses with STATUS_MORE_PROCESSING_REQUIRED must be zeros (no signature yet)
- The final SESSION_SETUP response IS signed but is NOT included in the hash chain
- For session binding, each new connection has its OWN preauth hash chain starting from that connection's NEGOTIATE

### 5. Protocol-Agnostic Client Tracking

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Unified ClientRecord model | Admins need to see who is connected, from where, via which protocol, with what identity. Currently NFS and SMB track connections independently with no unified view. | Medium | New model in `pkg/controlplane/models/`, runtime sub-service | `ClientRecord` should include: client IP, protocol (NFS/SMB), authenticated user, connected shares, connection time, last activity, active operations count. |
| `dfsctl client list` command | The primary admin interface for monitoring active connections. Must show all clients across all protocols in a single table. | Medium | New dfsctl command, REST API endpoint, runtime method | Table columns: IP, Protocol, User, Share, Connected Since, Last Activity, Ops/sec. Support `-o json` and `-o yaml`. |
| NFS mount tracking | NFS already has mount tracking in `mounts/` sub-service. Need to expose this as ClientRecord. | Low | `runtime/mounts/` service already tracks mounts | Transform existing mount data into ClientRecord format. |
| SMB session tracking | SMB session manager already tracks sessions with client address, username, creation time. Need to expose as ClientRecord. | Low | `session/manager.go` already tracks sessions | Transform existing session data into ClientRecord format. |
| Active operations per client | Useful for identifying chatty or stuck clients. | Low | Session credits already track `activeRequests` per session | NFS: count in-flight requests per connection. SMB: already tracked in credits. |
| Real-time metrics per client | Bytes read/written, operations per second. | Medium | Per-client counters, Prometheus metrics | Nice-to-have for v1. Start with connection-level tracking. |

**How production servers handle client tracking:**
- Windows Server: `Get-SmbSession`, `Get-SmbOpenFile` cmdlets. Shows user, client, share, open files, connection time.
- Samba: `smbstatus` shows PIDs, usernames, machine names, connected shares, protocol version. `net status sessions` for JSON output.
- Linux knfsd: `/proc/fs/nfsd/exports`, `showmount -a`. Tracks by IP + export path. No user info (AUTH_UNIX is spoofable).
- ONTAP: `vserver cifs session show`, `vserver nfs connected-clients show`. Unified cross-protocol view.

### 6. WPTS Conformance Fixes (73 Known -> Lower)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| CHANGE_NOTIFY full implementation | 20 of 73 known failures are ChangeNotify tests. The infrastructure exists (NotifyRegistry, async callbacks) but the dispatch handler returns STATUS_NOT_IMPLEMENTED instead of registering the watcher. | High | `change_notify.go` (infrastructure built), `dispatch.go` (handler wiring), async response framing | The heavy lifting is done: NotifyRegistry with path matching, async callbacks, rename pairing, recursive watch. Missing: (1) wiring the dispatch handler to register watchers, (2) hooking metadata mutations (create/remove/rename/setattr) to call NotifyChange, (3) async response framing (INTERIM_RESPONSE + later async completion). |
| Negotiate/Encryption fix candidates (5 tests) | BVT_Negotiate_SMB311 and encryption variants. Likely preauth hash or cipher negotiation bugs. | Medium | `negotiate.go`, `hooks.go`, `crypto_state.go` | These are the same preauth integrity hash issues that affect macOS signing. Fixing one likely fixes all 5. |
| Leasing/DurableHandle fix candidates (6 tests) | BVT_Leasing_FileLeasingV1/V2 (2), BVT_DurableHandleV1_Reconnect (2), BVT_DirectoryLeasing (2). Infrastructure exists but edge cases fail. | Medium | Lease handler, durable handle reconnect, directory leasing | Need WPTS log analysis to identify exact failure points. Likely timing or state machine issues. |
| ADS tests (9 tests) | Alternate Data Streams already implemented (Phase 43). Some tests still fail. | Medium | ADS in metadata layer | Per memory notes: ADS stored as directory children, share mode enforcement across base file + streams. Fix candidates need investigation. |
| Timestamp algorithm fixes (3 tests) | Freeze/unfreeze timestamp behavior for directories. Windows has specific rules about when ChangeTime auto-updates after SetFileAttributes(-1). | Low | `set_info.go`, `applyFrozenTimestamps` | Need to match MS-FSA 2.1.5.14.2 exactly for directory timestamp propagation. |
| TreeMgmt disconnect fix (1 test) | BVT_TreeMgmt_SMB311_Disconnect_NoSignedNoEncryptedTreeConnect. Likely requires allowing unsigned TREE_DISCONNECT in certain conditions. | Low | `tree_disconnect.go` | Minor protocol compliance fix. |

**Expected WPTS score improvement:**
- ChangeNotify: +20 passing (20 tests)
- Negotiate/Encryption: +5 passing (fix preauth hash)
- Leasing/Durable: +4-6 passing (edge case fixes)
- Timestamp: +2-3 passing
- TreeMgmt: +1 passing
- **Realistic target: 73 known -> ~40-45 known** (reduce by ~30)

---

## Differentiators

Features that set DittoFS apart. Not expected by most users, but highly valued when present.

### 7. Trash / Soft-Delete with Configurable Retention

| Feature | Value Proposition | Complexity | Dependencies | Notes |
|---------|-------------------|------------|--------------|-------|
| Server-side trash with per-share config | Protection against accidental deletion. Unlike client-side recycle bins, this works across all protocols and clients. NFS `rm` and SMB `del` both go to trash. | High | Metadata service intercept on RemoveFile/RemoveDirectory, new `.trash` directory convention, retention policy model | Most NAS vendors offer this: Synology (`@Recycle` per share), QNAP (Network Recycle Bin), ASUSTOR, Alibaba Cloud NAS. Must be transparent: moved files are invisible via NFS/SMB but visible via admin API. |
| Configurable retention (1-180 days) | Admins set how long deleted files are preserved. After TTL, garbage collection permanently removes them. | Low | Share config field, background GC goroutine | Simple timer-based cleanup. Older files deleted first when storage pressure exists. |
| Admin restore via API/CLI | `dfsctl trash list /export`, `dfsctl trash restore /export/file.txt` | Medium | REST API endpoint, metadata service trash operations | Move-from-trash reverses the original move. Must handle name conflicts (file already exists at original path). |
| Exclude patterns | Skip certain file extensions from trash (e.g., `.tmp`, `.log`, `.swp`). Reduces trash bloat from temporary files. | Low | Glob pattern matching in RemoveFile intercept | Standard NAS feature. Synology supports this. |
| Quota-aware trash | Trash counts against share quota. When share is full, oldest trash items are purged first to make space. | Medium | Quota enforcement integration | Without this, a share could be "full" even though most space is trash that could be freed. |

**How production NAS servers implement trash:**
- **Synology DSM**: `@Recycle` hidden folder per shared folder. Configurable empty schedule. Admin can empty all recycle bins at once.
- **QNAP**: Network Recycle Bin. Per-share enable/disable. Auto-delete after N days.
- **TrueNAS**: Uses ZFS snapshots for recovery rather than a trash folder approach.
- **ONTAP**: Relies on snapshots rather than per-file trash. `volume snapshot restore` for recovery.
- **Alibaba Cloud NAS**: Dedicated recycle bin feature. Files recoverable within configurable period.

**DittoFS approach:**
- Use a hidden directory (e.g., `.dfs-trash/`) within each share's metadata store
- On delete: move file to `.dfs-trash/{original-path}/{timestamp}-{name}`
- Preserve original path for restore
- Background goroutine scans trash and removes expired items
- Trash items are invisible in READDIR/ReadDirectory (filtered by metadata service)
- Admin API/CLI provides full visibility and restore

**Edge cases:**
- Hard links: deleting a hard-linked file should only trash if last link
- Directories: must recursively trash all contents
- Cross-share moves: not applicable (trash is per-share)
- Trash-of-trash: deleting from trash should permanently delete
- Concurrent restore + delete: need atomic move-from-trash
- Disk space: trash must count toward quota; when share is full, oldest trash purged

### 8. SMB Multi-Channel (Session Binding)

| Feature | Value Proposition | Complexity | Dependencies | Notes |
|---------|-------------------|------------|--------------|-------|
| Session binding across connections | Allows a single SMB session to span multiple TCP connections. Provides: aggregate bandwidth (multiple NICs), fault tolerance (one connection drops, others continue), load balancing. | Very High | Session manager changes, per-connection preauth hash, channel signing keys, connection-session binding model | This is a fundamental architectural change. Currently one connection = one session. Multi-channel means N connections share 1 session with independent signing keys per channel. |
| SMB2_SESSION_FLAG_BINDING in SESSION_SETUP | Client sends SESSION_SETUP with binding flag + existing SessionId on a new connection. Server validates auth and adds a new channel to the session. | High | `session_setup.go`, session manager, preauth hash per connection | Must validate: existing session in GlobalSessionTable, same user identity, SMB2_SESSION_FLAG_BINDING flag set, derive new channel signing key using new connection's preauth hash. |
| Per-channel signing keys | Each channel has its own signing key derived from the connection's preauth integrity hash. Responses on a channel are signed with that channel's key. | High | `session/crypto_state.go`, signing module | Session.ChannelList entries each have their own SigningKey. KDF uses the channel's connection PreauthIntegrityHashValue. |
| Interface advertisement (NETWORK_INTERFACE_INFO) | IOCTL to advertise server network interfaces. Client uses this to establish additional channels on different interfaces. | Medium | IOCTL handler in `stub_handlers.go` | Returns list of server interfaces with speed, capability flags, IP addresses. Without this, clients cannot discover multi-channel opportunities. |
| Negotiation: SMB2_GLOBAL_CAP_MULTI_CHANNEL | Server advertises multi-channel capability in NEGOTIATE response. Currently NOT set. | Low | `negotiate.go` capability flags | Simple flag, but should only be set when multi-channel is actually implemented and tested. |

**How Samba implements multi-channel:**
- Experimental in Samba for several years, becoming production-ready circa 2024-2025.
- `server multi channel support = yes` in smb.conf.
- Each smbd process handles one connection; session state is shared via ctdb (clustered TDB).
- Per-channel signing key stored in session's channel list.
- FSCTL_QUERY_NETWORK_INTERFACE_INFO returns interface list.

**Why this is a differentiator, not table stakes:**
- Many Go-based SMB servers and even some commercial NAS products do NOT support multi-channel.
- macOS Finder does not use multi-channel (Apple's SMB client does not initiate session binding).
- Linux smbclient is adding multi-channel support incrementally.
- Windows clients DO use multi-channel when available, gaining 2-4x throughput on multi-NIC setups.

**Recommendation: Defer to a later milestone.** Multi-channel is the most complex feature in this list and has the lowest ROI for single-instance deployments. It becomes valuable only with multiple NICs and high-throughput requirements. Implement the interface advertisement IOCTL first (low effort, no behavioral change), then tackle session binding in a dedicated milestone.

---

## Anti-Features

Features to explicitly NOT build in v0.10.0.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| User/group quotas (per-user limits) | Massively complex: requires tracking per-user usage across all shares, user identification on NFS is spoofable via AUTH_UNIX. Share quotas cover 90% of use cases. | Implement per-share quotas only. User quotas can be added later if demand exists. |
| Client-side recycle bin integration | Windows Recycle Bin uses `$RECYCLE.BIN` folder with per-SID subdirectories. Supporting this requires SID management, hidden folder conventions, and client-specific behavior. | Server-side trash is protocol-agnostic and simpler. Works identically for NFS and SMB clients. |
| SMB compression (LZ77, LZNT1, Pattern_V1) | 69 WPTS tests are skipped because they require compression. Implementing SMB compression is a large effort for marginal benefit (network is rarely the bottleneck for file servers on modern networks). | Leave as "skipped" in WPTS. Focus compression effort on BlockStore compression (v4.5) which benefits all protocols. |
| SMB persistent handles (reconnect after server restart) | Different from durable handles (which survive connection loss). Persistent handles require handle state to survive server restart, meaning serialization to disk and recovery on startup. | Durable handles already implemented. Persistent handles add enormous complexity for a feature only used with continuous availability (clustering). |
| RQUOTA protocol (RPC program 100011) | Traditional NFS quota reporting protocol. Linux `quota` command uses this. But it is a separate RPC program requiring its own wire protocol implementation. | Report quotas via FSSTAT/FSINFO (already in the protocol). `df` command will show correct quota-limited values without RQUOTA. |
| DFS referrals | 2 WPTS tests require DFS (Distributed File System referrals). DFS is a Windows-specific namespace feature that adds little value to DittoFS's architecture. | Leave as permanent known failures. DFS is out of scope per PROJECT.md. |
| Full SMB multi-channel in v0.10.0 | Session binding requires fundamental architectural changes (shared session state across connections, per-channel signing). Risk of destabilizing existing single-channel SMB. | Implement IOCTL interface advertisement only. Full multi-channel in a future milestone. |

---

## Feature Dependencies

```
Share Quotas --> Payload Stats (UsedSize must be accurate for quota enforcement)
Share Quotas --> FSSTAT/FSINFO Reporting (quotas must be reflected in protocol responses)
Payload Stats --> BlockStore.Stats() accuracy (engine must track real sizes)

WPTS Conformance --> SMB 3.1.1 Signing Fix (5 negotiate tests depend on correct preauth hash)
WPTS Conformance --> CHANGE_NOTIFY implementation (20 tests)

Client Tracking --> Mount tracking (NFS, already exists)
Client Tracking --> Session tracking (SMB, already exists)

Trash/Soft-Delete --> Share Quotas (trash must count against quota)
Trash/Soft-Delete --> Metadata service changes (intercept RemoveFile)

SMB Multi-Channel --> Credit flow control (credits are per-connection, must work correctly first)
SMB Multi-Channel --> SMB 3.1.1 Signing Fix (per-channel signing keys depend on preauth hash)
```

## MVP Recommendation

**Phase 1 - Foundation (must-do first):**
1. **Payload Stats** (UsedSize accuracy) -- everything else depends on this
2. **SMB 3.1.1 Signing Fix** (preauth hash) -- unblocks macOS clients AND 5 WPTS tests
3. **SMB Credit Flow Control** (validation/enforcement) -- protocol correctness

**Phase 2 - Core Production Features:**
4. **Share Quotas** with FSSTAT/FSINFO/SMB reporting -- admin-requested, depends on #1
5. **Protocol-Agnostic Client Tracking** -- operational visibility
6. **WPTS Conformance Fixes** -- ChangeNotify (20 tests) + leasing/durable edge cases

**Phase 3 - Enhancement:**
7. **Trash / Soft-Delete** -- valuable but not blocking production use
8. **SMB Multi-Channel** -- interface advertisement IOCTL only; full session binding deferred

**Defer entirely:**
- Full SMB multi-channel session binding (separate milestone)

## Sources

- [MS-SMB2: Algorithm for the Granting of Credits](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/2e366edb-b006-47e7-aa94-ef6f71043ced) -- Credit grant requirements (HIGH confidence)
- [MS-SMB2: Verifying the Credit Charge and Payload Size](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fba3123b-f566-4d8f-9715-0f529e856d25) -- Credit charge validation (HIGH confidence)
- [MS-SMB2: Granting Credits to the Client](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46256e72-b361-4d73-ac7d-d47c04b32e4b) -- Grant rules (HIGH confidence)
- [MS-SMB2: Handling Session Binding](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9a697646-6085-4597-808c-765bb2280c6e) -- Multi-channel session binding (HIGH confidence)
- [SMB 3.1.1 Pre-authentication integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- Test vectors for preauth hash (HIGH confidence)
- [SMB2 Credits - SambaWiki](https://wiki.samba.org/index.php/SMB2_Credits) -- Samba credit implementation reference (MEDIUM confidence)
- [NetApp: How Quotas display with NFS clients df output](https://kb.netapp.com/Advice_and_Troubleshooting/Data_Storage_Software/ONTAP_OS/How_Quotas_display_with_NFS_clients_df_output) -- Quota reporting behavior (MEDIUM confidence)
- [Seagate NAS OS: Network Recycle Bin](https://www.seagate.com/support/kb/nas-os-4x-network-recycle-bin-nrb-006005en/) -- Trash implementation pattern (MEDIUM confidence)
- [Synology DSM Recycle Bin](https://mariushosting.com/synology-how-to-empty-all-recycle-bins-on-dsm-7/) -- Per-share recycle bin pattern (MEDIUM confidence)
- [Samba Multi-Channel presentation (SNIA)](https://www.snia.org/educational-library/samba-multi-channel-iouring-status-update-2020) -- Multi-channel implementation complexity (MEDIUM confidence)
- DittoFS codebase analysis: `session/credits.go`, `session/manager.go`, `query_info.go`, `change_notify.go`, `crypto_state.go`, `hooks.go` -- Existing infrastructure assessment (HIGH confidence)
- WPTS KNOWN_FAILURES.md -- Current conformance status: 193 pass / 73 known / 0 new / 69 skipped (HIGH confidence)
