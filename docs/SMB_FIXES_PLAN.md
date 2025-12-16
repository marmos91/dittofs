# SMB2 Implementation Fixes Plan

## Current Status (2025-12-16 - Updated)

### Working Operations
- ✅ mkdir (CREATE with directory flag)
- ✅ create file (CREATE)
- ✅ read file (READ)
- ✅ list directory (QUERY_DIRECTORY)
- ✅ append to file (WRITE)
- ✅ copy file (CREATE + READ + WRITE)
- ✅ stat file (QUERY_INFO)
- ✅ large file write (100KB+)
- ✅ nested directories
- ✅ symlink creation (CREATE with reparse point)
- ✅ metadata persistence across restarts (BadgerDB)
- ✅ **rename/move** - Fixed with FileRenameInformation (SET_INFO class 10)
- ✅ **delete files** - Fixed with FileDispositionInformation (SET_INFO class 13)
- ✅ **delete directories (rmdir)** - Fixed via delete-on-close pattern
- ✅ **touch (update timestamps)** - Fixed with FileBasicInformation (SET_INFO class 4)
- ✅ **truncate/overwrite** - Fixed with FileEndOfFileInformation (SET_INFO class 20)

### Partially Working / Client Limitations
- ⚠️ symlink display → Shows as regular file on macOS SMB client (client limitation)
- ⚠️ readlink → macOS SMB client doesn't send FSCTL_GET_REPARSE_POINT (server implementation complete)

---

## Completed Fixes

### Fix 1: File Delete (FileDispositionInformation) ✅

**Files Modified:**
- `internal/protocol/smb/v2/handlers/handler.go` - Added `DeletePending`, `ParentHandle`, `FileName` to OpenFile struct
- `internal/protocol/smb/v2/handlers/create.go` - Store parent info for delete-on-close
- `internal/protocol/smb/v2/handlers/set_info.go` - Handle FileDispositionInformation (class 13) and FileDispositionInformationEx (class 64)
- `internal/protocol/smb/v2/handlers/close.go` - Perform actual deletion when DeletePending is set
- `internal/protocol/smb/types/constants.go` - Added FileDispositionInformation constants

**Implementation:**
- SET_INFO marks file for deletion via `openFile.DeletePending = true`
- CLOSE handler performs actual deletion using `metadataStore.RemoveFile()` or `RemoveDirectory()`
- Supports both class 13 (1-byte flag) and class 64 (4-byte flags field)

### Fix 2: File Rename/Move (FileRenameInformation) ✅

**Files Modified:**
- `internal/protocol/smb/v2/handlers/encoding.go` - Added `DecodeFileRenameInfo()` function
- `internal/protocol/smb/v2/handlers/set_info.go` - Handle FileRenameInformation (class 10)

**Implementation:**
- Decodes FILE_RENAME_INFORMATION structure (ReplaceIfExists, FileName in UTF-16LE)
- Supports simple rename (same directory) and move (different directory)
- Normalizes Windows backslashes to forward slashes
- Calls `metadataStore.Move()` for the actual operation

### Fix 3: Timestamp Updates (FileBasicInformation) ✅

**Files Modified:**
- `internal/protocol/smb/v2/handlers/converters.go` - `SMBTimesToSetAttrs()` handles special values (0, -1)

**Implementation:**
- Properly converts FILETIME to Go time
- Ignores zero and -1 values (meaning "don't change")
- Calls `metadataStore.SetFileAttributes()` with converted times

### Fix 4: SET_INFO FileID Injection for Compound Requests ✅

**Files Modified:**
- `pkg/adapter/smb/smb_connection.go` - Added SET_INFO to FileID injection condition and switch case

**Issue:** Compound requests (CREATE + SET_INFO + CLOSE) weren't injecting the FileID from CREATE into SET_INFO.

**Fix:** Added `types.SMB2SetInfo` to:
1. The condition in `processRequestWithInheritedFileID()` that decides when to inject FileID
2. The switch case in `injectFileID()` to inject at offset 16-32

### Fix 5: Proper SET_INFO Response Encoding ✅

**Files Modified:**
- `internal/protocol/smb/v2/handlers/set_info.go` - Use `EncodeSetInfoResponse()` instead of empty bytes

**Issue:** SET_INFO was returning `make([]byte, 0)` instead of proper 2-byte response structure.

**Fix:** Created `setInfoSuccessResponse()` helper that uses `EncodeSetInfoResponse()` to return proper response.

### Fix 6: Readlink via FSCTL_GET_REPARSE_POINT ✅ (Server-side complete)

**Files Modified:**
- `internal/protocol/smb/v2/handlers/stub_handlers.go` - Implemented `handleGetReparsePoint()`
- `internal/protocol/smb/types/status.go` - Added `StatusNotAReparsePoint`

**Implementation:**
- Added `FsctlGetReparsePoint` constant (0x000900A8)
- Implemented `handleGetReparsePoint()` to read symlink target
- Implemented `buildSymlinkReparseBuffer()` for SYMBOLIC_LINK_REPARSE_DATA_BUFFER
- Implemented `buildIoctlResponse()` for SMB2 IOCTL response

**Note:** macOS SMB client doesn't send this IOCTL for `readlink` command. Server implementation is complete but untested due to client limitation.

---

## Known Limitations

### macOS SMB Client Symlink Handling

macOS SMB client has limited support for symlinks over SMB:
1. **Symlink display**: Files with `FILE_ATTRIBUTE_REPARSE_POINT` still show as regular files in `ls -la`
2. **Readlink**: macOS doesn't send `FSCTL_GET_REPARSE_POINT` IOCTL for `readlink` command

The server correctly:
- Creates symlinks with proper metadata type
- Sets `FILE_ATTRIBUTE_REPARSE_POINT` in directory listings
- Implements FSCTL_GET_REPARSE_POINT handler

But the client doesn't interpret these correctly.

---

## Testing Results

All tests passed:

```bash
# Delete file - ✅ PASS
echo "test" > /tmp/smb/delete_test.txt
rm /tmp/smb/delete_test.txt

# Delete directory - ✅ PASS
mkdir /tmp/smb/rmdir_test
rmdir /tmp/smb/rmdir_test

# Rename - ✅ PASS
echo "test" > /tmp/smb/original.txt
mv /tmp/smb/original.txt /tmp/smb/renamed.txt

# Move to different directory - ✅ PASS
mv /tmp/smb/renamed.txt /tmp/smb/test_dir/

# Touch (timestamps) - ✅ PASS
touch /tmp/smb/test_dir/renamed.txt
# Access time updated correctly

# Symlink creation - ✅ PASS
ln -s target.txt /tmp/smb/mylink
# Symlink created but shows as regular file (client limitation)

# Readlink - ❌ Client doesn't send IOCTL
readlink /tmp/smb/mylink
# Fails due to macOS SMB client not supporting this
```

---

## References

- [MS-SMB2] SMB2 Protocol: https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/
- [MS-FSCC] File System Control Codes: https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/
- SET_INFO: MS-SMB2 2.2.39
- FileRenameInformation: MS-FSCC 2.4.34
- FileDispositionInformation: MS-FSCC 2.4.11
- FileBasicInformation: MS-FSCC 2.4.7
- REPARSE_DATA_BUFFER: MS-FSCC 2.1.2.1
