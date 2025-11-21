# Prometheus Metrics Refactoring - Progress Status

**Last Updated:** 2025-11-20
**Branch:** feat/prometheus

## Project Goal

Re-instrument Prometheus metrics for DittoFS with:
- Simple configuration (port + enabled boolean)
- Focus on adapters (NFS specifically)
- Track performance (throughput/latency), consistency (error rates), stability
- Share-level metrics for Grafana dropdown selectors
- Proper NFS status codes (not Go error strings)

## Completed Work

### 1. Metrics Interface and Implementation ✅

**Files Created/Modified:**
- `/pkg/metrics/nfs.go` - Interface definition with noop implementation
- `/pkg/metrics/prometheus/nfs.go` - Prometheus implementation
- `/pkg/config/metrics.go` - Factory pattern for metrics initialization

**Key Features:**
```go
// Metrics interface methods
RecordRequest(procedure, share, duration, errorCode)
RecordRequestStart(procedure, share)
RecordRequestEnd(procedure, share)
RecordBytesTransferred(procedure, share, direction, bytes uint64)
RecordOperationSize(operation, share, bytes uint64)
SetActiveConnections(count int32)
RecordConnectionAccepted()
RecordConnectionClosed()
RecordConnectionForceClosed()
```

**Metric Labels:**
- `procedure` - NFS procedure name (GETATTR, READ, WRITE, etc.)
- `share` - Share name for multi-share tracking
- `status` - "success" or "error"
- `error_code` - NFS error code string (e.g., "NFS3ERR_NOENT")
- `direction` - "read" or "write" for byte transfer
- `operation` - Operation type for size tracking

**Histogram Buckets:**
- Duration: 1ms, 10ms, 100ms, 1s, 10s (milliseconds)
- Size: 4KB, 64KB, 1MB, 10MB (bytes)

### 2. DittoServer Metrics Integration ✅

**Files Modified:**
- `/pkg/server/server.go` - Added `metricsServer *metrics.Server` field
- `/pkg/server/server.go` - Added `SetMetricsServer()` method
- `/pkg/server/server.go` - Integrated metrics server lifecycle (start/stop)
- `/cmd/dittofs/main.go` - Clean factory pattern integration

**Pattern:**
```go
metricsResult := config.InitializeMetrics(cfg)
if metricsResult.Server != nil {
    dittoSrv.SetMetricsServer(metricsResult.Server)
}
adapters, err := config.CreateAdapters(cfg, metricsResult.NFSMetrics)
```

### 3. Share Extraction at Connection Layer ✅

**Files Modified:**
- `/pkg/adapter/nfs/nfs_connection.go`

**Key Methods:**
```go
// Extracts share name from file handle at connection layer
func (c *NFSConnection) extractShareName(ctx context.Context, data []byte) (string, error)

// Clean logging helper
func (c *NFSConnection) logNFSRequest(procedure, share string, authCtx *nfs.NFSAuthContext)
```

**Benefits:**
- Handlers don't need to re-parse file handles for share
- Share available early for metrics
- Cleaner separation of concerns

### 4. Share in Auth Context ✅

**Files Modified:**
- `/internal/protocol/nfs/dispatch.go`

**Changes:**
```go
type NFSAuthContext struct {
    Context    context.Context
    ClientAddr string
    Share      string  // NEW: extracted at connection layer
    AuthFlavor uint32
    UID        *uint32
    GID        *uint32
    GIDs       []uint32
}

func ExtractAuthContext(
    ctx context.Context,
    call *rpc.RPCCallMessage,
    clientAddr string,
    share string,  // NEW parameter
    procedure string,
) *NFSAuthContext
```

### 5. HandlerResult Structure ✅

**Files Modified:**
- `/internal/protocol/nfs/dispatch.go`

**Structure Defined:**
```go
type HandlerResult struct {
    // Data contains the XDR-encoded response to send to the client
    Data      []byte

    // NFSStatus is the NFS protocol status code (for metrics/observability)
    NFSStatus uint32
}
```

**Handler Signature Updated:**
```go
type nfsProcedureHandler func(
    authCtx *NFSAuthContext,
    handler *nfs.Handler,
    reg *registry.Registry,
    data []byte,
) (*HandlerResult, error)  // Changed from ([]byte, error)

type mountProcedureHandler func(
    authCtx *NFSAuthContext,
    handler *mount.Handler,
    reg *registry.Registry,
    data []byte,
) (*HandlerResult, error)  // Changed from ([]byte, error)
```

### 6. Status Extraction Helpers ✅

**Files Modified:**
- `/internal/protocol/nfs/dispatch.go`

**Functions Created:**
```go
// Extract NFS status from XDR response (first 4 bytes, big-endian)
func extractNFSStatus(data []byte) uint32

// Extract Mount protocol status
func extractMountStatus(data []byte) uint32
```

### 7. All NFS Handler Wrappers Updated ✅

**Files Modified:**
- `/internal/protocol/nfs/dispatch.go` (lines 441-1328)

**Handlers Updated (20 total):**
- handleNFSNull
- handleNFSGetAttr
- handleNFSSetAttr
- handleNFSLookup
- handleNFSAccess
- handleNFSReadLink
- handleNFSRead
- handleNFSWrite
- handleNFSCreate
- handleNFSMkdir
- handleNFSSymlink
- handleNFSMknod
- handleNFSRemove
- handleNFSRmdir
- handleNFSRename
- handleNFSLink
- handleNFSReadDir
- handleNFSReadDirPlus
- handleNFSFsStat
- handleNFSFsInfo
- handleNFSPathConf
- handleNFSCommit

**Pattern Applied:**
```go
func handleNFSGetAttr(...) (*HandlerResult, error) {
    // ... create context ...

    responseData, err := handleRequest(...)

    if err != nil {
        return &HandlerResult{
            Data:      responseData,
            NFSStatus: types.NFS3ErrIO,
        }, err
    }

    status := extractNFSStatus(responseData)
    return &HandlerResult{
        Data:      responseData,
        NFSStatus: status,
    }, nil
}
```

## Work Completed - Session 2025-11-20

### All Handler Wrappers Updated ✅

**Files to Modify:**
- `/internal/protocol/nfs/dispatch.go` (lines 1090-1248)

**Mount Handlers to Update (6 total):**
- handleMountNull (line 1090)
- handleMountMnt (line 1119)
- handleMountDump (line 1155)
- handleMountUmnt (line 1179)
- handleMountUmntAll (line 1203)
- handleMountExport (line 1227)

**Apply Same Pattern:**
```go
func handleMountMnt(...) (*HandlerResult, error) {
    // ... existing logic ...

    responseData, err := handleRequest(...)

    if err != nil {
        return &HandlerResult{
            Data:      responseData,
            NFSStatus: mount.MountErrIO,
        }, err
    }

    status := extractMountStatus(responseData)
    return &HandlerResult{
        Data:      responseData,
        NFSStatus: status,
    }, nil
}
```

## Remaining Work

### 1. Update Connection Layer to Use HandlerResult

**File:** `/pkg/adapter/nfs/nfs_connection.go`

**Current State:** Connection layer expects handlers to return `([]byte, error)`

**Required Changes:**
Find where handlers are called and update to:
```go
// Old:
responseData, err := handler(authCtx, nfsHandler, registry, data)

// New:
result, err := handler(authCtx, nfsHandler, registry, data)
responseData := result.Data
nfsStatus := result.NFSStatus

// Use nfsStatus for metrics
c.nfsMetrics.RecordRequest(procedure, share, duration, nfsStatusToString(nfsStatus))
```

**Search Pattern:** Look for calls to `NfsDispatchTable[procedure].Handler(...)` and `MountDispatchTable[procedure].Handler(...)`

### 2. Create NFS Status to String Helper

**File:** `/internal/protocol/nfs/types/errors.go` or new file `/internal/protocol/nfs/status_string.go`

**Purpose:** Convert NFS status codes to metric-friendly strings

**Required Implementation:**
```go
package nfs

import "github.com/marmos91/dittofs/internal/protocol/nfs/types"

// NFSStatusToString converts NFS status codes to metric labels
func NFSStatusToString(status uint32) string {
    switch status {
    case types.NFS3OK:
        return "NFS3_OK"
    case types.NFS3ErrPerm:
        return "NFS3ERR_PERM"
    case types.NFS3ErrNoEnt:
        return "NFS3ERR_NOENT"
    case types.NFS3ErrIO:
        return "NFS3ERR_IO"
    case types.NFS3ErrNxio:
        return "NFS3ERR_NXIO"
    case types.NFS3ErrAcces:
        return "NFS3ERR_ACCES"
    case types.NFS3ErrExist:
        return "NFS3ERR_EXIST"
    case types.NFS3ErrXDev:
        return "NFS3ERR_XDEV"
    case types.NFS3ErrNoDev:
        return "NFS3ERR_NODEV"
    case types.NFS3ErrNotDir:
        return "NFS3ERR_NOTDIR"
    case types.NFS3ErrIsDir:
        return "NFS3ERR_ISDIR"
    case types.NFS3ErrInval:
        return "NFS3ERR_INVAL"
    case types.NFS3ErrFbig:
        return "NFS3ERR_FBIG"
    case types.NFS3ErrNoSpc:
        return "NFS3ERR_NOSPC"
    case types.NFS3ErrRofs:
        return "NFS3ERR_ROFS"
    case types.NFS3ErrMlink:
        return "NFS3ERR_MLINK"
    case types.NFS3ErrNameTooLong:
        return "NFS3ERR_NAMETOOLONG"
    case types.NFS3ErrNotEmpty:
        return "NFS3ERR_NOTEMPTY"
    case types.NFS3ErrDQuot:
        return "NFS3ERR_DQUOT"
    case types.NFS3ErrStale:
        return "NFS3ERR_STALE"
    case types.NFS3ErrRemote:
        return "NFS3ERR_REMOTE"
    case types.NFS3ErrBadHandle:
        return "NFS3ERR_BADHANDLE"
    case types.NFS3ErrNotSync:
        return "NFS3ERR_NOT_SYNC"
    case types.NFS3ErrBadCookie:
        return "NFS3ERR_BAD_COOKIE"
    case types.NFS3ErrNotSupp:
        return "NFS3ERR_NOTSUPP"
    case types.NFS3ErrTooSmall:
        return "NFS3ERR_TOOSMALL"
    case types.NFS3ErrServerFault:
        return "NFS3ERR_SERVERFAULT"
    case types.NFS3ErrBadType:
        return "NFS3ERR_BADTYPE"
    case types.NFS3ErrJukebox:
        return "NFS3ERR_JUKEBOX"
    default:
        return fmt.Sprintf("UNKNOWN_%d", status)
    }
}

// MountStatusToString converts Mount protocol status codes to metric labels
func MountStatusToString(status uint32) string {
    // Import mount package constants
    // Similar switch statement for mount status codes
    return fmt.Sprintf("MOUNT_%d", status)
}
```

**Check Required:** Look at `/internal/protocol/nfs/types/` to find all status code constants

### 3. Remove Redundant Share Extraction from Handlers

**Files:** All handler files in `/internal/protocol/nfs/v3/handlers/` and `/internal/protocol/nfs/mount/handlers/`

**Search Pattern:** Look for code that extracts share from file handle within handler functions

**What to Remove:**
- Any calls to `registry.GetShareNameForHandle()`
- Any file handle parsing for share extraction
- Any share validation that's now done at connection layer

**Note:** Share is now available via `authCtx.Share` in all handlers

### 4. Test Compilation

**Command:** `go build -o dittofs cmd/dittofs/main.go`

**Expected Issues:**
- Connection layer calls to handlers will fail (return type mismatch)
- Any direct handler calls not updated

**Fix Strategy:**
- Update all call sites to use `result.Data` and `result.NFSStatus`
- Pass `nfsStatus` to metrics recording

### 5. Update Metrics Recording in Connection Layer

**File:** `/pkg/adapter/nfs/nfs_connection.go`

**Current Pattern:**
```go
c.nfsMetrics.RecordRequest(procedure, share, duration, errorString)
```

**New Pattern:**
```go
errorCode := NFSStatusToString(result.NFSStatus)
c.nfsMetrics.RecordRequest(procedure, share, duration, errorCode)
```

**Status Determination:**
```go
status := "success"
if result.NFSStatus != types.NFS3OK {
    status = "error"
}
```

## Important Technical Notes

### NFS Status Codes Location
- **File:** `/internal/protocol/nfs/types/nfs.go`
- **Common Values:**
  - `types.NFS3OK = 0` (success)
  - `types.NFS3ErrNoEnt = 2` (file not found)
  - `types.NFS3ErrIO = 5` (I/O error)
  - `types.NFS3ErrAcces = 13` (permission denied)
  - `types.NFS3ErrStale = 70` (stale file handle)
  - `types.NFS3ErrBadHandle = 10001` (invalid handle)

### Mount Status Codes
- **File:** `/internal/protocol/nfs/mount/handlers/mount.go` or similar
- **Common Values:**
  - `mount.MountErrIO` - I/O error
  - Check the actual file for complete list

### XDR Response Format
- All NFS/Mount responses start with 4-byte status (big-endian uint32)
- `extractNFSStatus()` reads first 4 bytes: `data[0]<<24 | data[1]<<16 | data[2]<<8 | data[3]`

### Registry File Handle Encoding
- File handles encode share identity
- Format: RFC 1813 compliant (max 64 bytes)
- Registry method: `GetShareNameForHandle(ctx, handle)`

## Testing Checklist

- [ ] Mount handler wrappers return HandlerResult
- [ ] Connection layer updated to use HandlerResult
- [ ] NFSStatusToString() implemented
- [ ] Compilation succeeds: `go build cmd/dittofs/main.go`
- [ ] Start server with metrics enabled
- [ ] Verify metrics endpoint: `curl http://localhost:9090/metrics`
- [ ] Mount NFS share and perform operations
- [ ] Check metrics show correct status codes (not "error" strings)
- [ ] Verify share label appears in metrics
- [ ] Test multiple shares to ensure proper tracking
- [ ] Check Grafana can filter by share name

## Next Session Start Commands

```bash
cd /Users/marmos91/Projects/dittofs/ditto-prometheus

# Check current branch
git branch

# Review current changes
git status
git diff

# Read this file
cat PROMETHEUS_METRICS_REFACTOR.md

# Continue with mount handlers
# Open dispatch.go at line 1090 to see mount handlers
```

## Key Files Reference

**Metrics:**
- `/pkg/metrics/nfs.go` - Interface
- `/pkg/metrics/prometheus/nfs.go` - Implementation
- `/pkg/config/metrics.go` - Factory

**Dispatch:**
- `/internal/protocol/nfs/dispatch.go` - Handler wrappers, auth context, HandlerResult

**Connection:**
- `/pkg/adapter/nfs/nfs_connection.go` - Share extraction, handler calls, metrics recording

**Types:**
- `/internal/protocol/nfs/types/nfs.go` - NFS status codes
- `/internal/protocol/nfs/types/types.go` - NFS constants

**Main:**
- `/cmd/dittofs/main.go` - Integration point

## Configuration Example

```yaml
server:
  shutdown_timeout: 30s
  metrics:
    enabled: true
    port: 9090

adapters:
  nfs:
    enabled: true
    port: 2049

shares:
  - name: /export
    metadata_store: memory-meta
    content_store: memory-content
```

## Session 2025-11-20 Summary

### Completed Refactoring

The Prometheus metrics refactoring is now **COMPLETE**. All planned improvements have been implemented and the code compiles successfully.

#### Key Achievements

1. **Response Interface Enhanced** ✅
   - Added `GetStatus() uint32` method requirement to `rpcResponse` interface
   - All 28 response types now implement GetStatus()
   - NFS v3: 22 response types updated
   - Mount: 6 response types updated

2. **Status Field Standardization** ✅
   - Added `Status uint32` field to response types that lacked it:
     - `NullResponse` (NFS v3)
     - `NullResponse`, `DumpResponse`, `ExportResponse`, `UmountResponse`, `UmountAllResponse` (Mount)
   - All responses now consistently have Status field set to proper constants:
     - NFS responses use `types.NFS3OK`
     - Mount responses use `handlers.MountOK`

3. **Clean Handler Refactoring** ✅
   - `handleRequest()` in `utils.go` now returns `(*HandlerResult, error)`
   - Extracts status from response before encoding using `GetStatus()`
   - All 26 handler wrappers in `dispatch.go` simplified to just return `handleRequest()` directly
   - Eliminated 500+ lines of boilerplate code

4. **Status String Conversion** ✅
   - Created `/internal/protocol/nfs/status_string.go`
   - Implemented `NFSStatusToString()` - converts NFS status codes to metric labels
   - Implemented `MountStatusToString()` - converts Mount status codes to metric labels
   - Covers all status codes defined in constants:
     - 19 NFS v3 status codes
     - 10 Mount protocol status codes

5. **Metrics Integration** ✅
   - Updated `nfs_connection.go` to use NFS status codes instead of Go error strings
   - Both `handleNFSProcedure()` and `handleMountProcedure()` now record proper status codes
   - Metrics now show "NFS3_OK", "NFS3ERR_NOENT", "MOUNT_OK", etc. instead of generic error messages

6. **Share Extraction Cleanup** ✅
   - Removed redundant share extraction from all handlers
   - Share is now extracted once at connection layer and passed via context
   - Updated `BuildAuthContextWithMapping()` to accept share as parameter instead of extracting from file handle
   - Added `Share` field to all 16 handler context types
   - Added `GetShare()` method to NFSAuthContext interface
   - Removed `GetShareNameForHandle()` from RegistryAccessor interface
   - Cleaner separation of concerns - share routing happens at protocol layer, not in handlers

#### Files Modified

**New Files:**
- `/internal/protocol/nfs/status_string.go` - Status to string conversions

**Modified Files:**
- `/internal/protocol/nfs/utils.go` - handleRequest returns HandlerResult
- `/internal/protocol/nfs/dispatch.go` - All 26 handlers simplified
- `/pkg/adapter/nfs/nfs_connection.go` - Uses NFS status codes for metrics
- `/internal/protocol/nfs/v3/handlers/null.go` - Added Status field
- `/internal/protocol/nfs/v3/handlers/auth_helper.go` - BuildAuthContextWithMapping uses share parameter
- `/internal/protocol/nfs/v3/handlers/*.go` - Added Share field to 16 handler context types:
  - access.go, commit.go, create.go, link.go, lookup.go, mkdir.go, mknod.go
  - readdir.go, readdirplus.go, readlink.go, remove.go, rename.go, rmdir.go
  - setattr.go, symlink.go, write.go
- `/internal/protocol/nfs/mount/handlers/*.go` - All mount responses have Status field

#### Testing Status

- ✅ Compilation: Successful (`go build cmd/dittofs/main.go`)
- ✅ Binary: Created successfully (35MB)
- ✅ Help command: Works correctly
- ✅ Code formatting: `go fmt ./...` - no changes needed
- ✅ Static analysis: `go vet ./...` - no issues found
- ✅ Unit tests: All passing
- ✅ Integration tests: All passing
- ✅ E2E test build: Successful (tests build but require sudo to run)
- ⏳ Runtime testing: Not performed (next step)
- ⏳ Metrics validation: Not performed (next step)

#### Next Steps for Testing

1. **Start Server with Metrics**:
   ```bash
   ./dittofs start
   # Check that metrics server starts on port 9090
   ```

2. **Verify Metrics Endpoint**:
   ```bash
   curl http://localhost:9090/metrics | grep nfs
   # Should see metrics with proper status codes
   ```

3. **Mount and Test Operations**:
   ```bash
   # Mount the NFS share
   sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049,resvport localhost:/export /mnt/test

   # Perform various operations
   ls /mnt/test
   echo "test" > /mnt/test/file.txt
   cat /mnt/test/file.txt
   rm /mnt/test/file.txt

   # Check metrics again
   curl http://localhost:9090/metrics | grep nfs_requests_total
   ```

4. **Verify Status Codes in Metrics**:
   - Success operations should show `error_code="NFS3_OK"`
   - Failed operations should show specific codes like `error_code="NFS3ERR_NOENT"`
   - Mount operations should show `error_code="MOUNT_OK"`

5. **Grafana Dashboard**:
   - Import Prometheus data source
   - Create dashboard with share dropdown
   - Verify error rates by status code
   - Test filtering by share name

#### Benefits Achieved

1. **Clean Code**: Eliminated ~500 lines of repetitive boilerplate
2. **Type Safety**: Status extraction happens once in handleRequest()
3. **Better Metrics**: Prometheus now tracks proper NFS error codes, not Go errors
4. **Maintainability**: Adding new procedures requires minimal code
5. **RFC Compliance**: All responses properly track protocol status codes
6. **Observability**: Grafana can now show error rates by specific NFS error type
7. **Cleaner Architecture**: Share extraction happens once at connection layer, not repeated in every handler
8. **Performance**: Eliminated redundant share lookups from file handles in each handler call

## End of Status Document
