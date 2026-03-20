# Technology Stack: v0.10.0 Production Hardening + SMB Protocol Fixes

**Project:** DittoFS v0.10.0
**Researched:** 2026-03-20
**Overall Confidence:** HIGH (all features use existing Go stdlib + zero new dependencies)

## Executive Summary

v0.10.0 requires **zero new external dependencies**. Every feature -- SMB credit flow control, multi-channel session binding, share quotas, payload stats, client tracking, trash/soft-delete, macOS signing fix, and WPTS conformance fixes -- can be implemented using Go's standard library and the existing dependency set. The project already has the foundational infrastructure (session management, crypto state, metadata store interfaces, control plane models, WPTS Docker infrastructure) and this milestone is about hardening, fixing edge cases, and filling protocol gaps.

**Net impact: 0 new external dependencies, 0 new stdlib packages, ~2500-3500 LOC new code across 8 features.**

## Recommended Stack

### Core: No Changes Required

| Technology | Version | Purpose | Status |
|------------|---------|---------|--------|
| Go | 1.25.0 | Language | Already in go.mod |
| `crypto/sha512` (stdlib) | Go 1.25 | Preauth integrity hash (macOS fix) | Already used in `internal/adapter/smb/crypto_state.go` |
| `crypto/aes` + `crypto/cipher` (stdlib) | Go 1.25 | AES-CMAC/GMAC signing | Already used in `internal/adapter/smb/signing/` |
| `github.com/dgraph-io/badger/v4` | v4.5.2 | Metadata persistence | Already in go.mod |
| `gorm.io/gorm` | v1.31.1 | Control plane ORM | Already in go.mod |
| `github.com/go-chi/chi/v5` | v5.1.0 | REST API router | Already in go.mod |
| `github.com/spf13/cobra` | v1.8.1 | CLI framework | Already in go.mod |
| `github.com/stretchr/testify` | v1.11.1 | Testing | Already in go.mod |
| `github.com/hirochachacha/go-smb2` | v1.1.0 | SMB integration tests | Already in go.mod |

### Feature-Specific Stack Analysis

#### 1. SMB 3.1.1 Signing on macOS (Fix Preauth Integrity Hash Mismatch)

**Stack needed:** Nothing new. The issue is a bug in the preauth integrity hash chain computation, not a missing library.

| Component | Location | What Exists | What Needs Fixing |
|-----------|----------|-------------|-------------------|
| SHA-512 hash chain | `internal/adapter/smb/crypto_state.go` | `chainHash()`, `UpdatePreauthHash()` | Verify raw message bytes include complete NetBIOS-framed data that macOS expects |
| Per-session preauth | `internal/adapter/smb/crypto_state.go` | `InitSessionPreauthHash()`, `StashPendingSessionSetup()` | Verify hash ordering matches MS-SMB2 3.3.5.5 exactly (stash/consume timing) |
| KDF with preauth context | `internal/adapter/smb/kdf/kdf.go` | `DeriveKey()`, `LabelAndContext()` | Verify preauthHash passed is the SESSION-level hash, not CONNECTION-level |
| Key derivation | `internal/adapter/smb/session/crypto_state.go` | `DeriveAllKeys()` | Verify hash used is `GetSessionPreauthHash(sessionID)` not `GetPreauthHash()` |
| Dispatch hooks | `internal/adapter/smb/hooks.go` | Before/after hooks for NEGOTIATE and SESSION_SETUP | Verify compound SESSION_SETUP messages hash correctly |

**Root cause hypothesis (HIGH confidence):** The preauth hash fed to `DeriveAllKeys()` does not match what macOS computes because either: (a) the response bytes hashed by `sessionPreauthAfterHook` differ from what the client receives (e.g., after signing/padding modifications), or (b) the session-level vs connection-level hash is mixed up, or (c) the stash/consume sequence in `InitSessionPreauthHash` drops or double-counts a message. These are all fix-in-place changes requiring no new code.

**Testing:** `mount_smbfs //user@host/share /mnt` on macOS + Wireshark packet capture comparison. The `hirochachacha/go-smb2` client (already in go.mod) can also validate 3.1.1 signing.

**Confidence:** HIGH -- all crypto primitives exist, this is a correctness fix.

---

#### 2. Share Quotas with FSSTAT/FSINFO/SMB Reporting

**Stack needed:** Nothing new. Extend existing metadata store interface and control plane model.

| Component | Location | What Exists | What Needs Adding |
|-----------|----------|-------------|-------------------|
| Share model | `pkg/controlplane/models/share.go` | `Share` struct with GORM tags | Add `QuotaBytes int64` and `QuotaFiles int64` fields |
| Control plane store | `pkg/controlplane/store/` | `ShareStore` interface | Quota fields auto-migrate via GORM |
| Metadata filesystem stats | `pkg/metadata/types.go` | `FilesystemStatistics` with TotalBytes/UsedBytes/AvailableBytes | Wire quota into TotalBytes, compute AvailableBytes from quota - used |
| NFS FSSTAT handler | `internal/adapter/nfs/v3/handlers/fsstat.go` | Returns `FilesystemStatistics` | Already works -- quota flows through metadata service |
| NFS FSINFO handler | `internal/adapter/nfs/v3/handlers/fsinfo.go` | Returns `FilesystemCapabilities` | Already works |
| SMB QueryInfo handler | `internal/adapter/smb/v2/handlers/` | `FileFsFullSizeInformation`, `FileFsSizeInformation` | Wire quota values into SMB filesystem info responses |
| dfsctl CLI | `cmd/dfsctl/commands/share/` | Share create/update | Add `--quota-bytes` and `--quota-files` flags |
| REST API | `internal/controlplane/api/handlers/` | Share CRUD handlers | Quota fields propagate automatically via GORM model |

**Architecture decision:** Quotas are enforced in the metadata service layer (not protocol handlers) because both NFS and SMB must respect the same limits. The metadata service's `PrepareWrite()` checks quota before allowing writes. `GetFilesystemStatistics()` computes available space as `min(physicalAvailable, quotaRemaining)`.

**Confidence:** HIGH -- pure extension of existing interfaces.

---

#### 3. Payload Stats (UsedSize Returns Actual Storage Usage)

**Stack needed:** Nothing new. Fix the TODO in blockstore engine.

| Component | Location | What Exists | What Needs Fixing |
|-----------|----------|-------------|-------------------|
| BlockStore stats | `pkg/blockstore/store.go` | `StoreStats.UsedSize` field defined | Value is always 0 (TODO comment in engine.go:279) |
| Engine stats | `pkg/blockstore/engine/engine.go` | `Stats()` method | Implement by summing local store block sizes |
| Local FS store | `pkg/blockstore/local/fs/fs.go` | Block file management | Add `UsedBytes()` method (walk block dir or track incrementally) |
| Memory store | `pkg/blockstore/local/memory/memory.go` | In-memory blocks | Sum block sizes in Stats() |

**Implementation approach:** Track used bytes incrementally via atomic counter in `LocalStore` -- increment on `WriteBlock`, decrement on `DeleteBlock`. Avoids filesystem walks on the hot path.

**Confidence:** HIGH -- straightforward implementation of an existing interface field.

---

#### 4. Protocol-Agnostic Client Tracking with `dfsctl client list`

**Stack needed:** Nothing new. New runtime sub-service using existing patterns.

| Component | Location | What Exists | What Needs Adding |
|-----------|----------|-------------|-------------------|
| NFS mount tracking | `pkg/controlplane/runtime/mounts/` | Mount tracking per-share | Already tracks client IP and mount time |
| SMB session tracking | `internal/adapter/smb/session/manager.go` | Session with ClientAddr, CreatedAt | Already has all needed data |
| Runtime | `pkg/controlplane/runtime/runtime.go` | Composition layer with sub-services | Add `clients/` sub-service that aggregates NFS mounts + SMB sessions |
| REST API | `internal/controlplane/api/handlers/` | Pattern established for all CRUD | Add `GET /api/clients` endpoint |
| dfsctl CLI | `cmd/dfsctl/commands/` | Pattern established for all resources | Add `client/` subcommand directory |

**ClientRecord model:**
```go
type ClientRecord struct {
    ClientAddr    string    // IP address
    Protocol      string    // "nfs" or "smb"
    ShareName     string    // Mounted share
    Username      string    // Authenticated user (empty for NFS AUTH_SYS)
    ConnectedAt   time.Time // Session/mount start time
    LastActivity  time.Time // Last request timestamp
    SessionID     string    // Protocol-specific session identifier
}
```

**Integration:** The `clients/` sub-service polls `mounts.Service` (NFS) and the SMB adapter's `SessionManager` via a new `adapter.ClientProvider` interface. Adapters implement this interface to expose their active clients.

**Confidence:** HIGH -- follows established runtime sub-service pattern.

---

#### 5. Trash / Soft-Delete with Configurable Retention

**Stack needed:** Nothing new. Metadata-layer feature with background cleanup.

| Component | Location | What Exists | What Needs Adding |
|-----------|----------|-------------|-------------------|
| Metadata service | `pkg/metadata/service.go` | `RemoveFile()`, `RemoveDirectory()` | Add `MoveToTrash()` that renames to `.trash/<uuid>` instead of deleting |
| File struct | `pkg/metadata/types.go` | `File` with all attributes | Add `DeletedAt *time.Time` and `OriginalPath string` fields |
| Metadata store | `pkg/metadata/store.go` | `Files` interface | Add `ListTrash()` and `PurgeTrash()` methods |
| Share model | `pkg/controlplane/models/share.go` | `Share` struct | Add `TrashEnabled bool` and `TrashRetentionDays int` fields |
| GC | `pkg/blockstore/gc/gc.go` | Block garbage collection | Extend to purge trash entries past retention |
| dfsctl CLI | `cmd/dfsctl/commands/` | Pattern established | Add `trash list`, `trash restore`, `trash purge` subcommands |
| REST API | `internal/controlplane/api/handlers/` | Pattern established | Add `/api/shares/{name}/trash` endpoints |

**Architecture decision:** Trash is implemented at the metadata layer, not the protocol layer. Both NFS `REMOVE`/`RMDIR` and SMB `DELETE` flow through `MetadataService.RemoveFile()`, which checks the share's `TrashEnabled` setting and routes to either hard-delete or soft-delete.

**Trash directory:** `.trash/` is a hidden directory in each share's root. Files are renamed to `.trash/<original-name>.<uuid>` to avoid collisions. A metadata field `OriginalPath` stores the original path for restore operations.

**Retention enforcement:** A background goroutine (similar to the existing syncer/health monitor pattern) scans `.trash/` periodically and purges entries older than `TrashRetentionDays`.

**Confidence:** HIGH -- follows existing patterns, no external dependencies.

---

#### 6. SMB Credit Flow Control (Grant/Charge Accounting)

**Stack needed:** Nothing new. The infrastructure already exists -- this is about fixing protocol compliance.

| Component | Location | What Exists | What Needs Fixing |
|-----------|----------|-------------|-------------------|
| Credit constants | `internal/adapter/smb/session/credits.go` | `CreditConfig`, `CalculateCreditCharge()`, strategies | Complete |
| Session credits | `internal/adapter/smb/session/session.go` | `ConsumeCredits()`, `GrantCredits()`, `GetOutstanding()` | Complete |
| Session manager | `internal/adapter/smb/session/manager.go` | `GrantCredits()` with adaptive algorithm | Complete |
| Dispatch layer | `internal/adapter/smb/dispatch.go` + framing | Handler dispatching | **Missing:** Credit charge validation before dispatch (reject if insufficient credits) |
| Response building | `internal/adapter/smb/response.go` | Response sending | **Missing:** Set CreditResponse header field from `GrantCredits()` return |
| Multi-credit I/O | `internal/adapter/smb/v2/handlers/` | READ/WRITE handlers | **Missing:** `CreditCharge = ceil(length/65536)` validation for large I/O |

**What specifically needs to happen (per MS-SMB2 3.3.5.2.3):**

1. **Request validation:** Before dispatching, verify `CreditCharge` in the request header. For multi-credit operations (READ/WRITE with data > 64KB), charge must equal `ceil(PayloadSize / 65536)`. Reject with STATUS_INVALID_PARAMETER if `CreditCharge` < required.

2. **Credit consumption:** Call `session.ConsumeCredits(creditCharge)` during dispatch (already partially done but needs enforcement).

3. **Credit granting:** Call `sessionManager.GrantCredits(sessionID, requested, creditCharge)` and write the returned value into the response header's `CreditResponse` field.

4. **Insufficient credits:** If `session.GetOutstanding() < creditCharge`, return STATUS_REQUEST_NOT_ACCEPTED.

5. **MessageID validation (sequence window):** The CreditCharge defines a range of MessageIDs consumed: `[MessageID, MessageID + CreditCharge - 1]`. Verify all IDs in this range are within the granted window.

**Confidence:** HIGH -- credit infrastructure exists, needs protocol-level wiring.

---

#### 7. SMB Multi-Channel Session Binding

**Stack needed:** Nothing new. Extends session management with channel tracking.

| Component | Location | What Exists | What Needs Adding |
|-----------|----------|-------------|-------------------|
| Session | `internal/adapter/smb/session/session.go` | Single-connection session | Add `Channels []Channel` field, `ChannelSequence uint16` |
| Session manager | `internal/adapter/smb/session/manager.go` | Session CRUD | Add `BindSession(existingSessionID, newConn)` method |
| SESSION_SETUP handler | `internal/adapter/smb/v2/handlers/` | Auth flow | Handle `SMB2_SESSION_FLAG_BINDING` (0x01) flag |
| NEGOTIATE handler | `internal/adapter/smb/v2/handlers/` | Dialect negotiation | Advertise `CapMultiChannel` (0x00000008) capability |
| Signing verification | `internal/adapter/smb/framing.go` | Session signing verifier | Use `Channel.SigningKey` instead of `Session.SigningKey` for bound channels |
| ConnInfo | `internal/adapter/smb/conn_types.go` | Per-connection state | Track which sessions are bound to this connection |
| Adapter | `pkg/adapter/smb/adapter.go` | Session-to-connection mapping via `sessionConns sync.Map` | Extend to support multiple connections per session |

**Multi-channel data model:**
```go
type Channel struct {
    Connection  net.Conn
    SigningKey  []byte      // Per-channel signing key derived from session key
    Signer     signing.Signer
}
```

**Session binding flow (per MS-SMB2 3.3.5.5.3):**
1. Client sends SESSION_SETUP with `SMB2_SESSION_FLAG_BINDING` and existing `SessionId`
2. Server validates the session exists and the client authenticates with the same identity
3. Server derives a new `Channel.SigningKey` using the session's preauth hash for this channel
4. Server adds the channel to `Session.Channels`
5. Subsequent requests on this connection use the channel-specific signing key

**Key nuance:** The `Channel.SigningKey` for bound channels in SMB 3.1.1 uses the NEW connection's preauth integrity hash (not the original session's hash). This is critical for correctness.

**Capability advertisement:** The `CapMultiChannel` constant already exists at `internal/adapter/smb/types/constants.go:362`. It just needs to be set in NEGOTIATE response capabilities when multi-channel is enabled.

**Confidence:** MEDIUM -- well-documented in MS-SMB2 but requires careful implementation of channel-specific signing keys and concurrent session access patterns. No new libraries needed.

---

#### 8. WPTS Conformance Fixes (Reduce Known Failures from 73)

**Stack needed:** Nothing new. Bug fixes in existing handlers.

| Area | Current Known Failures | Likely Fixes | Location |
|------|----------------------|--------------|----------|
| ChangeNotify | ~20 tests (Phase 78 in MEMORY.md) | Wire `NotifyRegistry` into metadata operations (CREATE, REMOVE, RENAME, SETATTR) | `internal/adapter/smb/v2/handlers/` |
| Timestamp flaky | ~1 test | Fix timestamp precision or comparison tolerance | Various handlers |
| Remaining ~52 | Require unimplemented features | Skip/defer (compression, persistent handles, multi-channel, named pipes) | N/A |

**ChangeNotify integration points:**
The `NotifyRegistry` is already fully implemented with `NotifyChange()`, `NotifyRename()`, and async callback delivery. What is missing is calling it from the metadata operations:
- `MetadataService.CreateFile()` -> `NotifyChange(shareName, parentPath, fileName, FileActionAdded)`
- `MetadataService.RemoveFile()` -> `NotifyChange(shareName, parentPath, fileName, FileActionRemoved)`
- `MetadataService.Move()` -> `NotifyRename(shareName, oldParent, oldName, newParent, newName)`
- `MetadataService.SetFileAttributes()` -> `NotifyChange(shareName, parentPath, fileName, FileActionModified)`

This requires passing the `NotifyRegistry` reference into the metadata service or using an event/callback pattern.

**Confidence:** HIGH for ChangeNotify (infrastructure exists), MEDIUM for remaining fixes (depend on specific test failure analysis).

## Alternatives Considered

| Category | Recommended | Alternative | Why Not Alternative |
|----------|-------------|-------------|---------------------|
| Quota enforcement | Metadata service layer | Kernel quotactl via syscall | DittoFS is userspace; kernel quotas don't apply to virtual filesystems |
| Quota storage | GORM fields on Share model | Separate quota table | Single row per share is sufficient; no need for per-user quotas yet |
| Client tracking | Runtime sub-service aggregation | Prometheus metrics only | CLI needs structured data, not just counters |
| Trash storage | Hidden `.trash/` directory in share | Separate trash store | Keeps trash within share boundary, simpler backup/restore |
| Trash metadata | `DeletedAt` + `OriginalPath` on File | Separate trash record table | Fewer schema changes, atomic move within same store |
| Credit enforcement | Dispatch-layer validation | Handler-level validation | Credits are cross-cutting; dispatch layer is the correct interception point |
| Multi-channel | Session.Channels slice | Separate ChannelManager | Channels are session-scoped; keeping them on Session is simpler |
| ChangeNotify | Event callbacks from MetadataService | fsnotify on local filesystem | DittoFS metadata is in-memory/BadgerDB, not a local filesystem |
| macOS signing fix | Debug & fix hash chain ordering | Add macOS-specific workaround | Workarounds mask the real bug; fixing the hash chain is correct |

## What NOT to Add

These are things that might seem needed but are NOT required for v0.10.0:

| Anti-Recommendation | Why Not |
|---------------------|---------|
| External quota library | Go's `syscall.Statfs` is already used for disk stats; quotas are metadata-layer, not kernel-layer |
| `fsnotify` for ChangeNotify | DittoFS doesn't use a real filesystem for metadata; events originate from metadata operations |
| Separate credit manager package | Credit management is already unified in `session.Manager`; no need to split |
| New testing framework for WPTS | Docker-based WPTS infrastructure already exists from v3.6/v3.8 |
| `sync.Pool` for credit tracking | Credit operations are lightweight atomic ops; pooling adds complexity with no benefit |
| External trash/recycle library | `github.com/nao1215/trash` follows FreeDesktop spec for local desktop use; DittoFS needs server-side trash in its own metadata layer |
| Go 1.26 features | Go 1.25 is current and sufficient; no v0.10.0 features require newer language features |
| New crypto libraries | All signing/hashing/KDF code already exists; macOS fix is a correctness issue, not a missing algorithm |

## New Package/File Layout

```
pkg/controlplane/runtime/
  clients/                       # NEW: Protocol-agnostic client tracking sub-service
    service.go                   # ClientRecord aggregation from NFS mounts + SMB sessions
    service_test.go

pkg/metadata/
  types.go                       # MODIFY: Add DeletedAt, OriginalPath to File struct
  service.go                     # MODIFY: Add MoveToTrash(), ListTrash(), PurgeTrash()
  trash.go                       # NEW: Trash/soft-delete business logic (~150 lines)
  trash_test.go
  quota.go                       # NEW: Quota enforcement in PrepareWrite() (~100 lines)
  quota_test.go

pkg/controlplane/models/
  share.go                       # MODIFY: Add QuotaBytes, QuotaFiles, TrashEnabled, TrashRetentionDays

internal/adapter/smb/
  session/
    session.go                   # MODIFY: Add Channels []Channel, ChannelSequence
    channel.go                   # NEW: Channel type with per-channel signing key (~50 lines)
    channel_test.go
  v2/handlers/
    Various                      # MODIFY: Credit charge validation, ChangeNotify wiring, multi-channel binding

cmd/dfsctl/commands/
  client/                        # NEW: `dfsctl client list` command
    list.go
  trash/                         # NEW: `dfsctl trash list|restore|purge` commands
    list.go
    restore.go
    purge.go
```

## Integration Points with Existing Code

### Credit Flow Control Integration

```
Request arrives
  -> framing.go: ReadRequest() parses header with CreditCharge
  -> NEW: Validate CreditCharge against session outstanding credits
  -> NEW: Reject with STATUS_REQUEST_NOT_ACCEPTED if insufficient
  -> dispatch.go: Route to handler
  -> handler returns response
  -> NEW: response.go: Set CreditResponse = GrantCredits(sessionID, requested, charge)
  -> framing.go: Write response with CreditResponse header field
```

### Quota Integration

```
NFS/SMB write request
  -> MetadataService.PrepareWrite(handle, newSize)
  -> NEW: Check share quota (QuotaBytes - currentUsedBytes >= newWriteSize)
  -> Returns WriteOperation or ErrQuotaExceeded
  -> NFS FSSTAT: GetFilesystemStatistics() returns quota-adjusted AvailableBytes
  -> SMB QueryInfo FileFsFullSizeInformation: returns quota-adjusted values
```

### Client Tracking Integration

```
pkg/adapter/adapter.go:
  NEW: ClientProvider interface {
    ListClients() []ClientRecord
  }

NFS adapter implements ClientProvider via mount tracking
SMB adapter implements ClientProvider via session manager

runtime/clients/service.go aggregates all ClientProvider instances
REST API exposes /api/clients
dfsctl client list renders table
```

### Trash Integration

```
NFS REMOVE / SMB DELETE
  -> MetadataService.RemoveFile(ctx, parentHandle, name)
  -> Check share.TrashEnabled
  -> If true: MoveToTrash() (rename to .trash/<name>.<uuid>, set DeletedAt)
  -> If false: existing hard-delete path

Background goroutine (started by runtime):
  -> Every 5 minutes: scan .trash/ for entries older than TrashRetentionDays
  -> PurgeTrash() -> hard-delete + block GC
```

## Installation

```bash
# No new dependencies to install.
# All features use existing go.mod dependencies.
go mod tidy  # Clean up any unused dependencies
```

## Dependency Impact Summary

| Feature | New External Deps | New Stdlib Packages | Lines of Code (est.) |
|---------|-------------------|--------------------|--------------------|
| macOS signing fix | 0 | 0 | ~50 (fixes in existing code) |
| Share quotas | 0 | 0 | ~300 (model + service + handlers + CLI) |
| Payload stats | 0 | 0 | ~100 (engine stats tracking) |
| Client tracking | 0 | 0 | ~400 (sub-service + API + CLI) |
| Trash/soft-delete | 0 | 0 | ~500 (service + store + GC + API + CLI) |
| Credit flow control | 0 | 0 | ~200 (dispatch validation + response wiring) |
| Multi-channel | 0 | 0 | ~600 (channel type + session binding + signing) |
| WPTS fixes | 0 | 0 | ~300 (ChangeNotify wiring + handler fixes) |
| **Total** | **0** | **0** | **~2450** |

## Version Pinning

All existing dependencies remain at current versions. No upgrades needed for v0.10.0 features.

| Dependency | Current Version | Required Version | Notes |
|------------|----------------|-----------------|-------|
| Go | 1.25.0 | 1.25.0 | No upgrade needed |
| badger/v4 | 4.5.2 | 4.5.2 | Stable |
| gorm | 1.31.1 | 1.31.1 | Auto-migrate handles new fields |
| aws-sdk-go-v2 | 1.39.6 | 1.39.6 | Not involved in v0.10.0 |
| go-smb2 | 1.1.0 | 1.1.0 | Used for integration tests |

## Sources

**HIGH confidence (official MS-SMB2 specification):**
- [MS-SMB2 Credit Charge and Payload Size](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fba3123b-f566-4d8f-9715-0f529e856d25) -- CreditCharge validation rules
- [MS-SMB2 Algorithm for Granting Credits](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/2e366edb-b006-47e7-aa94-ef6f71043ced) -- Server credit grant algorithm
- [MS-SMB2 Granting Credits to Client](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46256e72-b361-4d73-ac7d-d47c04b32e4b) -- Credit response field semantics
- [MS-SMB2 Calculating the CreditCharge](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/18183100-026a-46e1-87a4-46013d534b9c) -- Multi-credit I/O charge formula
- [MS-SMB2 Session Binding](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9a697646-6085-4597-808c-765bb2280c6e) -- Multi-channel session binding procedure
- [MS-SMB2 Per Session State](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/8174c219-2224-4009-b96a-06d84eccb3ae) -- Session.ChannelList, ChannelSequence
- [MS-SMB2 SESSION_SETUP Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a3c2c28-d6b0-48ed-b917-a86b2ca4575f) -- SMB2_SESSION_FLAG_BINDING flag
- [MS-SMB2 Handling New Authentication](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/7fd079ca-17e6-4f02-8449-46b606ea289c) -- Session setup with binding
- [MS-SMB2 Preauth Integrity Capabilities](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) -- SHA-512 hash chain spec
- [SMB 3.1.1 Pre-authentication integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- Preauth hash chain details

**MEDIUM confidence (Samba wiki, community references):**
- [Samba SMB2 Credits](https://wiki.samba.org/index.php/SMB2_Credits) -- Practical credit flow implementation notes
- [NetApp SMB 3.0 Multichannel Technical Report](https://www.netapp.com/media/17136-tr4740.pdf) -- Multi-channel architecture and testing
- [Impacket SMB2 compound response signing fix](https://github.com/fortra/impacket/pull/1834) -- Compound request signing edge case (relevant to macOS fix)

**LOW confidence (general reference, not version-specific):**
- [Filesystem quota management in Go (ANEXIA Blog)](https://anexia.com/blog/en/filesystem-quota-management-in-go/) -- Go quotactl wrapper approach
- [github.com/nao1215/trash](https://pkg.go.dev/github.com/nao1215/trash/go-trash) -- Go FreeDesktop trash library (not used, but pattern reference)
