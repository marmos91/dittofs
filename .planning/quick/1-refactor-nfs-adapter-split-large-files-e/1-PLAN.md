---
phase: quick-1
plan: 01
type: execute
wave: 1
depends_on: []
files_modified:
  # Part 3: Shared XDR decoder
  - internal/protocol/nfs/xdr/decode_handle.go
  - internal/protocol/nfs/v3/handlers/lookup_codec.go
  - internal/protocol/nfs/v3/handlers/read_codec.go
  - internal/protocol/nfs/v3/handlers/write_codec.go
  - internal/protocol/nfs/v3/handlers/getattr_codec.go
  - internal/protocol/nfs/v3/handlers/setattr_codec.go
  - internal/protocol/nfs/v3/handlers/create_codec.go
  - internal/protocol/nfs/v3/handlers/mkdir_codec.go
  - internal/protocol/nfs/v3/handlers/remove_codec.go
  - internal/protocol/nfs/v3/handlers/rmdir_codec.go
  - internal/protocol/nfs/v3/handlers/rename_codec.go
  - internal/protocol/nfs/v3/handlers/link_codec.go
  - internal/protocol/nfs/v3/handlers/symlink_codec.go
  - internal/protocol/nfs/v3/handlers/readlink_codec.go
  - internal/protocol/nfs/v3/handlers/readdir_codec.go
  - internal/protocol/nfs/v3/handlers/readdirplus_codec.go
  - internal/protocol/nfs/v3/handlers/access_codec.go
  - internal/protocol/nfs/v3/handlers/fsinfo_codec.go
  - internal/protocol/nfs/v3/handlers/fsstat_codec.go
  - internal/protocol/nfs/v3/handlers/pathconf_codec.go
  - internal/protocol/nfs/v3/handlers/mknod_codec.go
  - internal/protocol/nfs/v3/handlers/commit_codec.go
  # Part 1: Split nfs_connection.go
  - pkg/adapter/nfs/nfs_connection.go
  - pkg/adapter/nfs/nfs_connection_dispatch.go
  - pkg/adapter/nfs/nfs_connection_handlers.go
  - pkg/adapter/nfs/nfs_connection_reply.go
  # Part 2: Split nfs_adapter.go
  - pkg/adapter/nfs/nfs_adapter.go
  - pkg/adapter/nfs/nfs_adapter_shutdown.go
  - pkg/adapter/nfs/nfs_adapter_nlm.go
  - pkg/adapter/nfs/nfs_adapter_settings.go
  # Part 4: Split dispatch.go
  - internal/protocol/nfs/dispatch.go
  - internal/protocol/nfs/dispatch_nfs.go
  - internal/protocol/nfs/dispatch_mount.go
  # Part 5: Fix READ/WRITE metrics
  - internal/protocol/nfs/v3/handlers/read.go
  - internal/protocol/nfs/v3/handlers/write.go
  # Part 6-7: Tests
  - internal/protocol/nfs/v3/handlers/readdirplus_test.go
  - internal/protocol/nfs/v3/handlers/link_test.go
  - internal/protocol/nfs/v3/handlers/symlink_test.go
  - internal/protocol/nfs/v3/handlers/readlink_test.go
  - internal/protocol/nfs/v3/handlers/commit_test.go
  - internal/protocol/nfs/v3/handlers/access_test.go
  - internal/protocol/nfs/v3/handlers/fsinfo_test.go
  - internal/protocol/nfs/v3/handlers/fsstat_test.go
  - internal/protocol/nfs/v3/handlers/pathconf_test.go
  - internal/protocol/nfs/v3/handlers/mknod_test.go
  - internal/protocol/nfs/v3/handlers/null_test.go
  - internal/protocol/nfs/dispatch_test.go
autonomous: true
requirements: []
must_haves:
  truths:
    - "All existing tests pass after every file split"
    - "No behavioral changes in parts 1-4 (pure structural refactoring)"
    - "READ/WRITE metrics use actual byte counts from handler responses instead of re-decoding requests"
    - "11 missing handler procedures have behavioral test coverage"
    - "Dispatch table completeness and ExtractHandlerContext are tested"
  artifacts:
    - path: "internal/protocol/nfs/xdr/decode_handle.go"
      provides: "Shared DecodeFileHandleFromReader and DecodeStringFromReader functions"
      exports: ["DecodeFileHandleFromReader", "DecodeStringFromReader"]
    - path: "pkg/adapter/nfs/nfs_connection_dispatch.go"
      provides: "handleRPCCall, GSS interception, program multiplexer"
    - path: "pkg/adapter/nfs/nfs_connection_handlers.go"
      provides: "Per-protocol handlers (handleNFSProcedure, handleMountProcedure, handleNLMProcedure, handleNSMProcedure, handleNFSv4Procedure)"
    - path: "pkg/adapter/nfs/nfs_connection_reply.go"
      provides: "writeReply, sendReply, sendGSSReply"
    - path: "pkg/adapter/nfs/nfs_adapter_shutdown.go"
      provides: "initiateShutdown, gracefulShutdown, forceCloseConnections, interruptBlockingReads"
    - path: "pkg/adapter/nfs/nfs_adapter_nlm.go"
      provides: "NLM/NSM initialization, processNLMWaiters, handleClientCrash"
    - path: "pkg/adapter/nfs/nfs_adapter_settings.go"
      provides: "applyNFSSettings, settings watcher"
    - path: "internal/protocol/nfs/dispatch_nfs.go"
      provides: "NfsDispatchTable, initNFSDispatchTable, 22 handleNFS* wrapper functions"
    - path: "internal/protocol/nfs/dispatch_mount.go"
      provides: "MountDispatchTable, initMountDispatchTable, 6 handleMount* wrapper functions"
    - path: "internal/protocol/nfs/dispatch_test.go"
      provides: "Tests for ExtractHandlerContext and dispatch table completeness"
  key_links:
    - from: "internal/protocol/nfs/v3/handlers/*_codec.go"
      to: "internal/protocol/nfs/xdr/decode_handle.go"
      via: "DecodeFileHandleFromReader / DecodeStringFromReader"
      pattern: "xdr\\.Decode(FileHandle|String)FromReader"
    - from: "internal/protocol/nfs/dispatch_nfs.go"
      to: "internal/protocol/nfs/dispatch.go"
      via: "HandlerResult, nfsProcedure types"
      pattern: "nfsProcedure|HandlerResult"
    - from: "pkg/adapter/nfs/nfs_connection_dispatch.go"
      to: "pkg/adapter/nfs/nfs_connection_handlers.go"
      via: "handleNFSProcedure, handleMountProcedure calls"
      pattern: "c\\.handle(NFS|Mount|NLM|NSM)Procedure"
---

<objective>
Refactor NFS adapter per issue #148: split 3 oversized files into focused modules, extract shared XDR decoder, fix READ/WRITE metrics double-decode, and add missing test coverage.

Purpose: Improve code maintainability by reducing file sizes from 1000-1300 lines to 150-500 lines each, eliminate duplicated XDR file handle decoding across 22 codec files, fix a performance issue where READ/WRITE metrics re-decode the entire request, and add test coverage for 11 untested NFS procedures plus dispatch table tests.

Output: Restructured NFS adapter with identical behavior, shared XDR decoder, fixed metrics, and comprehensive test coverage.
</objective>

<execution_context>
@/Users/marmos91/.claude/get-shit-done/workflows/execute-plan.md
@/Users/marmos91/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@CLAUDE.md
@pkg/adapter/CLAUDE.md
@internal/protocol/CLAUDE.md
@pkg/adapter/nfs/nfs_connection.go
@pkg/adapter/nfs/nfs_adapter.go
@internal/protocol/nfs/dispatch.go
@internal/protocol/nfs/utils.go
@internal/protocol/nfs/xdr/decode.go
@internal/protocol/nfs/xdr/filehandle.go
@internal/protocol/nfs/v3/handlers/testing/fixtures.go
@internal/protocol/nfs/v3/handlers/read.go
@internal/protocol/nfs/v3/handlers/write.go
@internal/protocol/nfs/v3/handlers/lookup_codec.go
@internal/protocol/nfs/v3/handlers/read_codec.go
</context>

<tasks>

<task type="auto">
  <name>Task 1: Extract shared XDR decoder and split 3 oversized files (Parts 1-4)</name>
  <files>
    internal/protocol/nfs/xdr/decode_handle.go
    internal/protocol/nfs/v3/handlers/*_codec.go (22 files)
    pkg/adapter/nfs/nfs_connection.go
    pkg/adapter/nfs/nfs_connection_dispatch.go
    pkg/adapter/nfs/nfs_connection_handlers.go
    pkg/adapter/nfs/nfs_connection_reply.go
    pkg/adapter/nfs/nfs_adapter.go
    pkg/adapter/nfs/nfs_adapter_shutdown.go
    pkg/adapter/nfs/nfs_adapter_nlm.go
    pkg/adapter/nfs/nfs_adapter_settings.go
    internal/protocol/nfs/dispatch.go
    internal/protocol/nfs/dispatch_nfs.go
    internal/protocol/nfs/dispatch_mount.go
  </files>
  <action>
    **CRITICAL: This is a pure structural refactor. No behavioral changes. Every function must stay in the same package. Only imports within the same package may change.**

    **All commits must be GPG-signed with `-S`. Do NOT mention Claude in any commit message.**

    Execute these 4 parts in order, verifying compilation after each:

    **Part 3 (first because it's a dependency simplifier): Extract shared XDR file handle decoder**

    Create `internal/protocol/nfs/xdr/decode_handle.go` with two functions:

    ```go
    // DecodeFileHandleFromReader decodes an XDR opaque file handle from an io.Reader.
    // Returns (handle, error). Handle is nil if length is 0.
    // Validates max 64 bytes per RFC 1813.
    func DecodeFileHandleFromReader(reader io.Reader) (metadata.FileHandle, error)

    // DecodeStringFromReader decodes an XDR string from an io.Reader.
    // Returns (string, error). Validates max length.
    func DecodeStringFromReader(reader io.Reader) (string, error)
    ```

    `DecodeFileHandleFromReader` consolidates the repeated pattern found in all 22 codec files: read handle length uint32, validate <= 64, read handle bytes, skip XDR padding. Use `DecodeOpaque` internally (it already exists in the same package via `xdr.DecodeOpaque`).

    `DecodeStringFromReader` wraps the existing `DecodeString` (already in decode.go) but adds a `maxLength` parameter for validation. Actually, just use `DecodeString` directly - the wrapper is `DecodeStringFromReader(reader) -> xdr.DecodeString(reader)` with the same signature. If the existing `DecodeString` in `decode.go` is sufficient (it delegates to `xdr.DecodeString`), codec files can just call `xdr.DecodeString(reader)` directly. The key extraction is `DecodeFileHandleFromReader`.

    Then update each of the 22 `*_codec.go` files in `internal/protocol/nfs/v3/handlers/` to replace their inline handle decoding (the ~15-line pattern of read handleLen, validate, read bytes, skip padding) with a call to `xdr.DecodeFileHandleFromReader(reader)`. Each codec file's Decode* function currently creates a `bytes.NewReader(data)`, then manually decodes the handle. Replace the manual handle decoding block with `handle, err := xdr.DecodeFileHandleFromReader(reader)`.

    For codec files that decode TWO handles (rename_codec.go, link_codec.go): replace both inline handle decode blocks.

    For codec files that also decode a string after the handle (lookup_codec.go, create_codec.go, mkdir_codec.go, remove_codec.go, rmdir_codec.go, symlink_codec.go, mknod_codec.go, rename_codec.go, link_codec.go): the string decoding can use the existing `xdr.DecodeString(reader)` which is already available.

    Verify: `go build ./...` and `go test ./internal/protocol/nfs/...`
    Commit: `refactor: extract shared XDR file handle decoder (issue #148 part 3)` with `-S` flag.

    **Part 1: Split nfs_connection.go (1286 lines) into 4 files**

    All files stay in `package nfs` under `pkg/adapter/nfs/`.

    - `nfs_connection.go` (~300 lines): Keep `NFSConnection` struct, `fragmentHeader`, `NewNFSConnection`, `Serve`, `readRequest`, `processRequest`, `readFragmentHeader`, `readRPCMessage`, `handleUnsupportedVersion`, `handleConnectionClose`, `handleRequestPanic`. Keep all the struct fields and the request reading/serving loop.

    - `nfs_connection_dispatch.go` (~250 lines): Move `handleRPCCall` (the big switch on call.Program, lines 323-529) and `extractShareName`. This file contains the RPC program multiplexer and GSS interception logic.

    - `nfs_connection_handlers.go` (~350 lines): Move `handleNFSProcedure`, `handleMountProcedure`, `handleNLMProcedure`, `handleNSMProcedure`, `handleNFSv4Procedure`, `isOperationBlocked`, `makeBlockedOpResponse`. These are the per-protocol dispatch methods.

    - `nfs_connection_reply.go` (~150 lines): Move `sendReply`, `writeReply`, `sendGSSReply`. These are the reply/write methods.

    All methods are on `*NFSConnection` receiver so they share the struct naturally. Imports must be split correctly -- each new file only imports what it actually uses. Remove any unused imports from the shrunk nfs_connection.go.

    Verify: `go build ./...` and `go test ./pkg/adapter/nfs/...`
    Commit: `refactor: split nfs_connection.go into focused modules (issue #148 part 1)` with `-S` flag.

    **Part 2: Split nfs_adapter.go (1335 lines) into 4 files**

    All files stay in `package nfs` under `pkg/adapter/nfs/`.

    - `nfs_adapter.go` (~500 lines): Keep `NFSAdapter` struct and all its fields, `NFSConfig` struct, `NFSTimeoutsConfig` struct, `applyDefaults`, `validate`, `New`, `SetKerberosConfig`, `SetRuntime`, `Serve` (accept loop), `Port`, `Protocol`, `GetActiveConnections`, `GetListenerAddr`, `newConn`, `logMetrics`, `logV3FirstUse`, `logV4FirstUse`.

    - `nfs_adapter_shutdown.go` (~200 lines): Move `initiateShutdown`, `interruptBlockingReads`, `gracefulShutdown`, `forceCloseConnections`, `Stop`.

    - `nfs_adapter_nlm.go` (~250 lines): Move `processNLMWaiters`, `getLockManagerForHandle`, `initNSMHandler`, `handleClientCrash`, `performNSMStartup`, `initGSSProcessor`.

    - `nfs_adapter_settings.go` (~50 lines): Move `applyNFSSettings`.

    All methods are on `*NFSAdapter` receiver. Split imports accordingly.

    Verify: `go build ./...` and `go test ./pkg/adapter/nfs/...`
    Commit: `refactor: split nfs_adapter.go into focused modules (issue #148 part 2)` with `-S` flag.

    **Part 4: Split dispatch.go (989 lines) into 3 files**

    All files stay in `package nfs` under `internal/protocol/nfs/`.

    - `dispatch.go` (~160 lines): Keep `HandlerResult` struct (lines 28-53), `ExtractHandlerContext` function (lines 86-157), type definitions (`nfsProcedureHandler`, `nfsProcedure`, `mountProcedureHandler`, `mountProcedure`), the exported vars `NfsDispatchTable` and `MountDispatchTable`, the `init()` function, and the `handleRequest` generic function from `utils.go`. Actually, `handleRequest` is in `utils.go` which is a separate file -- leave it there.

    - `dispatch_nfs.go` (~500 lines): Move `initNFSDispatchTable` and all 22 `handleNFS*` wrapper functions (handleNFSNull through handleNFSCommit, lines 254-829). Also move the `NFSStatusToString` helper if it exists in this file (check -- it might be in a different file).

    - `dispatch_mount.go` (~250 lines): Move `initMountDispatchTable` and all 6 `handleMount*` wrapper functions (handleMountNull through handleMountExport, lines 835-989). Also move `MountStatusToString` helper if in this file.

    The `init()` function in dispatch.go calls `initNFSDispatchTable()` and `initMountDispatchTable()` -- these are package-level functions so they can be called across files within the same package. The `init()` stays in dispatch.go.

    Check if `NFSStatusToString` and `MountStatusToString` are in dispatch.go or another file -- they are referenced in nfs_connection_handlers.go as `nfs.NFSStatusToString` and `nfs.MountStatusToString`. Grep for their definitions and move them to the appropriate dispatch file.

    Verify: `go build ./...` and `go test ./internal/protocol/nfs/...`
    Commit: `refactor: split dispatch.go into focused modules (issue #148 part 4)` with `-S` flag.
  </action>
  <verify>
    After each part:
    ```
    go build ./...
    go test ./pkg/adapter/nfs/... ./internal/protocol/nfs/... -count=1
    ```
    After all 4 parts complete:
    ```
    go vet ./...
    go test ./... -count=1 -timeout 5m
    ```
    File line counts: `wc -l` on each new file should show no file exceeds ~500 lines. The original 3 files should each be reduced significantly.
  </verify>
  <done>
    - `nfs_connection.go` reduced from 1286 to ~300 lines, split into 4 files
    - `nfs_adapter.go` reduced from 1335 to ~500 lines, split into 4 files
    - `dispatch.go` reduced from 989 to ~160 lines, split into 3 files
    - 22 codec files use shared `DecodeFileHandleFromReader` instead of inline decoding
    - All existing tests pass unchanged
    - `go build ./...` succeeds
    - 4 GPG-signed commits, one per part
  </done>
</task>

<task type="auto">
  <name>Task 2: Fix READ/WRITE metrics double-decode (Part 5)</name>
  <files>
    internal/protocol/nfs/dispatch_nfs.go
    internal/protocol/nfs/v3/handlers/read.go
    internal/protocol/nfs/v3/handlers/write.go
    internal/protocol/nfs/v3/handlers/nfs_response.go
  </files>
  <action>
    **Fix the performance issue where READ/WRITE dispatch wrappers re-decode the entire request just to extract byte counts for metrics.**

    Currently in `dispatch_nfs.go` (was dispatch.go), `handleNFSRead` calls `handleRequest(...)` then re-decodes the request with `nfs.DecodeReadRequest(data)` to get `req.Count` for BytesRead. Similarly `handleNFSWrite` re-decodes to get `len(req.Data)` for BytesWritten. This wastes CPU on a hot path.

    **Fix approach**: Have the Read() and Write() handlers set BytesRead/BytesWritten on their response structs. Then the dispatch wrappers read them from the response instead of re-decoding.

    1. Add fields to response structs:
       - In `read.go`, add `ActualBytesRead uint64` field to `ReadResponse` struct. Set it in the `Read()` handler to `uint64(len(resp.Data))` on success.
       - In `write.go`, add `ActualBytesWritten uint64` field to `WriteResponse` struct. Set it in the `Write()` handler to `uint64(req.Count)` (the count from the already-decoded request) on success.

    2. Update the `handleNFSRead` wrapper in `dispatch_nfs.go`:
       - Remove the post-handler re-decode block (lines that call `nfs.DecodeReadRequest(data)` after `handleRequest`)
       - Instead, after `handleRequest`, decode the response to get actual bytes. But wait -- `handleRequest` returns `*HandlerResult` with encoded bytes. We cannot easily decode back.
       - **Better approach**: Change the pattern. The `handleRequest` generic function returns `HandlerResult` with `NFSStatus`. We need to also propagate BytesRead/BytesWritten. Options:
         a. Add BytesRead/BytesWritten to the response interface -- but that changes the generic
         b. Keep the current dispatch pattern but have Read/Write set the values on HandlerResult via a different mechanism
         c. **Simplest fix**: In `handleNFSRead` and `handleNFSWrite`, don't use the generic `handleRequest`. Instead, decode+handle+encode manually (3 lines each) and set BytesRead/BytesWritten directly. This avoids the double-decode entirely.
       - Actually, the simplest approach given the existing code: modify `handleNFSRead` to capture the response BEFORE encoding. Currently handleRequest does decode->handle->encode in one shot. We need the response object between handle and encode.

       **Recommended approach**: Add a `BytesTransferred` field to `ReadResponse` and `WriteResponse`. Then in the dispatch wrapper, after `handleRequest` returns, check `result.NFSStatus == NFS3OK` and if so, instead of re-decoding the request, use a lightweight extraction. Actually, looking more carefully:

       The cleanest fix per the issue description: "Have Read()/Write() handlers set BytesRead/BytesWritten on response structs instead of re-decoding in dispatch."

       So the approach is:
       1. Add a method `GetBytesRead() uint64` to ReadResponse and `GetBytesWritten() uint64` to WriteResponse
       2. The `handleRequest` generic already has access to the response before encoding. We can add a post-handle hook.

       **Actually, the simplest and most correct approach:**

       Don't use the generic `handleRequest` for READ and WRITE. Instead, write custom dispatch wrappers that:
       1. Decode the request
       2. Call handler.Read/Write
       3. Get actual byte count from the response (resp.Count for reads, len(req.Data) for writes)
       4. Encode the response
       5. Set result.BytesRead / result.BytesWritten

       This is ~20 lines per handler vs the current pattern of generic + post-decode hack.

       In `dispatch_nfs.go`, replace `handleNFSRead`:
       ```go
       func handleNFSRead(...) (*HandlerResult, error) {
           req, err := nfs.DecodeReadRequest(data)
           if err != nil {
               // encode error response
               errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
               encoded, _ := errResp.Encode()
               return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
           }
           resp, err := handler.Read(ctx, req)
           if err != nil {
               errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
               encoded, _ := errResp.Encode()
               return &HandlerResult{Data: encoded, NFSStatus: types.NFS3ErrIO}, err
           }
           status := resp.GetStatus()
           encoded, err := resp.Encode()
           // Release pooled resources after encoding
           if releaser, ok := any(resp).(nfs.Releaser); ok {
               releaser.Release()
           }
           if err != nil {
               errResp := &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: types.NFS3ErrIO}}
               encodedErr, _ := errResp.Encode()
               return &HandlerResult{Data: encodedErr, NFSStatus: types.NFS3ErrIO}, err
           }
           result := &HandlerResult{Data: encoded, NFSStatus: status}
           if status == types.NFS3OK {
               result.BytesRead = uint64(resp.Count)  // Actual bytes from handler, no re-decode
           }
           return result, nil
       }
       ```

       Similarly for `handleNFSWrite`, using `uint64(resp.Count)` or the actual write count from the response.

       Check what fields `WriteResponse` has -- it should have a `Count` field for bytes written. Use that.

    Commit: `fix: eliminate READ/WRITE metrics double-decode in dispatch (issue #148 part 5)` with `-S` flag.
  </action>
  <verify>
    ```
    go build ./...
    go test ./internal/protocol/nfs/... -count=1
    go test ./internal/protocol/nfs/v3/handlers/... -count=1 -run "TestRead|TestWrite"
    ```
  </verify>
  <done>
    - `handleNFSRead` and `handleNFSWrite` no longer call DecodeReadRequest/DecodeWriteRequest twice
    - BytesRead uses `resp.Count` (actual count from handler), BytesWritten uses response count
    - All existing read/write tests pass
    - No double-decode on the hot path
  </done>
</task>

<task type="auto">
  <name>Task 3: Add missing behavioral tests and dispatch tests (Parts 6-7)</name>
  <files>
    internal/protocol/nfs/v3/handlers/readdirplus_test.go
    internal/protocol/nfs/v3/handlers/link_test.go
    internal/protocol/nfs/v3/handlers/symlink_test.go
    internal/protocol/nfs/v3/handlers/readlink_test.go
    internal/protocol/nfs/v3/handlers/commit_test.go
    internal/protocol/nfs/v3/handlers/access_test.go
    internal/protocol/nfs/v3/handlers/fsinfo_test.go
    internal/protocol/nfs/v3/handlers/fsstat_test.go
    internal/protocol/nfs/v3/handlers/pathconf_test.go
    internal/protocol/nfs/v3/handlers/mknod_test.go
    internal/protocol/nfs/v3/handlers/null_test.go
    internal/protocol/nfs/dispatch_test.go
  </files>
  <action>
    **Part 6: Add 11 missing behavioral tests using HandlerTestFixture**

    All tests use `testing_helpers.NewHandlerFixture(t)` from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`. Follow the exact pattern of existing tests (e.g., `getattr_test.go`, `lookup_test.go`, `readdir_test.go`). Each test file tests the handler method directly (e.g., `fixture.Handler.ReadDirPlus(ctx, req)`) with real memory stores.

    Import the testing package as: `testing_helpers "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"`

    For each test file, examine the corresponding handler (e.g., `readdirplus.go`) and existing similar tests to understand the request/response structures.

    Create these test files:

    1. **`readdirplus_test.go`**: Test ReadDirPlus handler.
       - `TestReadDirPlus_EmptyDirectory` - root dir returns "." and ".." only
       - `TestReadDirPlus_WithFiles` - dir with files returns entries with attributes
       - `TestReadDirPlus_InvalidHandle` - returns NFS3ERR_STALE

    2. **`link_test.go`**: Test Link (hard link) handler.
       - `TestLink_Success` - create hard link, verify link count increases
       - `TestLink_DirectoryFails` - linking a directory returns NFS3ERR_ISDIR or error
       - `TestLink_InvalidHandle` - returns NFS3ERR_STALE

    3. **`symlink_test.go`**: Test Symlink handler.
       - `TestSymlink_Success` - create symlink, verify it exists
       - `TestSymlink_DuplicateName` - creating symlink with existing name fails

    4. **`readlink_test.go`**: Test ReadLink handler.
       - `TestReadLink_Success` - read symlink target
       - `TestReadLink_NotSymlink` - reading non-symlink returns error

    5. **`commit_test.go`**: Test Commit handler.
       - `TestCommit_Success` - commit on valid file handle succeeds
       - `TestCommit_InvalidHandle` - returns NFS3ERR_STALE

    6. **`access_test.go`**: Test Access handler.
       - `TestAccess_RootFile` - check access bits for a file
       - `TestAccess_Directory` - check access bits for a directory

    7. **`fsinfo_test.go`**: Test FsInfo handler.
       - `TestFsInfo_Success` - returns valid fs info with rtmax, wtmax, dtpref

    8. **`fsstat_test.go`**: Test FsStat handler.
       - `TestFsStat_Success` - returns valid fs stats

    9. **`pathconf_test.go`**: Test PathConf handler.
       - `TestPathConf_Success` - returns valid path config (max name length, etc.)

    10. **`mknod_test.go`**: Test Mknod handler.
        - `TestMknod_Success` - create a character/block device node (or verify it returns appropriate error since memory store may not support device nodes)

    11. **`null_test.go`**: Test Null handler.
        - `TestNull_Success` - null procedure returns successfully (no-op ping)

    Each test should:
    - Use `fixture := testing_helpers.NewHandlerFixture(t)`
    - Create necessary files/dirs with `fixture.CreateFile()` / `fixture.CreateDirectory()`
    - Call handler method: `resp, err := fixture.Handler.XXX(fixture.Context(), req)`
    - Assert response status and key fields
    - Use `types.NFS3OK` for success checks

    **Part 7: Add dispatch_test.go**

    Create `internal/protocol/nfs/dispatch_test.go` with:

    1. **`TestExtractHandlerContext_AuthUnix`** - verify UID/GID extraction from AUTH_UNIX call
    2. **`TestExtractHandlerContext_AuthNull`** - verify nil credentials for AUTH_NULL
    3. **`TestExtractHandlerContext_EmptyAuthBody`** - verify graceful handling
    4. **`TestNFSDispatchTable_Completeness`** - verify all 22 NFSv3 procedures are in NfsDispatchTable (NULL through COMMIT)
    5. **`TestMountDispatchTable_Completeness`** - verify all 6 Mount procedures are in MountDispatchTable
    6. **`TestNFSDispatchTable_AuthRequirements`** - verify NeedsAuth flags are correct (NULL/GETATTR/FSSTAT/FSINFO/PATHCONF/MKNOD should be false, others true)

    For ExtractHandlerContext tests, construct mock `rpc.RPCCallMessage` objects. Check the RPC types to see how to construct them -- you may need to use `rpc.NewCallMessage()` or manually construct the struct. Use the real `ExtractHandlerContext` function from the `nfs` package (`internal/protocol/nfs`).

    For dispatch table tests, iterate the tables and verify entries by procedure number using `types.NFSProc*` constants.

    Commit: `test: add missing handler and dispatch tests (issue #148 parts 6-7)` with `-S` flag.
  </action>
  <verify>
    ```
    go test ./internal/protocol/nfs/v3/handlers/... -count=1 -v -run "TestReadDirPlus|TestLink|TestSymlink|TestReadLink|TestCommit|TestAccess|TestFsInfo|TestFsStat|TestPathConf|TestMknod|TestNull"
    go test ./internal/protocol/nfs/... -count=1 -v -run "TestExtract|TestNFSDispatch|TestMountDispatch"
    ```
    All new tests pass. No existing tests broken.
  </verify>
  <done>
    - 11 new test files in `internal/protocol/nfs/v3/handlers/` covering all previously untested procedures
    - `dispatch_test.go` tests ExtractHandlerContext with AUTH_UNIX and AUTH_NULL, plus dispatch table completeness
    - All tests pass with `go test ./... -count=1`
    - GPG-signed commit
  </done>
</task>

</tasks>

<verification>
After all 3 tasks complete:
```bash
# Full build
go build ./...

# Full test suite
go test ./... -count=1 -timeout 10m

# Verify no file exceeds ~500 lines in split targets
wc -l pkg/adapter/nfs/nfs_connection*.go pkg/adapter/nfs/nfs_adapter*.go internal/protocol/nfs/dispatch*.go

# Verify vet passes
go vet ./...
```
</verification>

<success_criteria>
1. All 3 original oversized files split into focused modules (11 total files)
2. 22 codec files use shared DecodeFileHandleFromReader
3. READ/WRITE metrics no longer double-decode requests
4. 11 new handler test files + 1 dispatch test file
5. All existing tests pass unchanged
6. `go build ./...` and `go vet ./...` clean
7. 6 GPG-signed commits on refactor/148-nfs-adapter-cleanup branch
</success_criteria>

<output>
After completion, create `.planning/quick/1-refactor-nfs-adapter-split-large-files-e/1-01-SUMMARY.md`
</output>
