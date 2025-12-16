# SMB2 Implementation Fixes Plan

## Current Status (2025-12-16)

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

### Not Working Operations
- ❌ rename/move → "Bad file descriptor"
- ❌ delete → "Bad file descriptor"
- ❌ truncate/overwrite existing file → "rPC struct is bad"
- ❌ touch (update timestamps) → "Bad file descriptor"
- ❌ readlink → fails
- ❌ symlink display → shows as regular file instead of `lrwx------`

---

## Fix 1: Implement File Rename (FileRenameInformation)

**File:** `internal/protocol/smb/v2/handlers/set_info.go`

**Current code (line ~105-109):**
```go
case types.FileRenameInformation:
    // FILE_RENAME_INFORMATION [MS-FSCC] 2.4.34
    // TODO: Implement rename operation using metadataStore.Move()
    logger.Debug("SET_INFO: rename not implemented", "path", openFile.Path)
    return NewResult(types.StatusSuccess, make([]byte, 0)), nil
```

**MS-FSCC 2.4.34 FILE_RENAME_INFORMATION structure:**
```
ReplaceIfExists (1 byte)     - If TRUE, replace existing file
Reserved (7 bytes)           - Must be ignored
RootDirectory (8 bytes)      - Usually 0 (same volume)
FileNameLength (4 bytes)     - Length of FileName in bytes
FileName (variable)          - New name (UTF-16LE, may be full path or relative)
```

**Implementation steps:**
1. Add `DecodeFileRenameInfo(buffer []byte)` in `encoding.go`
2. Parse the rename info structure
3. Extract new filename (UTF-16LE → string)
4. Determine if it's a rename (same directory) or move (different directory)
5. Call `metadataStore.Move(authCtx, fromDir, fromName, toDir, toName)`
6. Return appropriate status

**Edge cases:**
- Relative vs absolute paths
- ReplaceIfExists flag handling
- Cross-directory moves
- Renaming directories

---

## Fix 2: Implement File Delete (FileDispositionInformation)

**File:** `internal/protocol/smb/v2/handlers/set_info.go`

**Current code (line ~111-114):**
```go
case 13: // FileDispositionInformation [MS-FSCC] 2.4.11
    // TODO: Implement delete on close
    logger.Debug("SET_INFO: delete disposition not implemented", "path", openFile.Path)
    return NewResult(types.StatusSuccess, make([]byte, 0)), nil
```

**MS-FSCC 2.4.11 FILE_DISPOSITION_INFORMATION structure:**
```
DeletePending (1 byte) - If non-zero, delete file when all handles closed
```

**SMB2 delete flow:**
1. Client opens file with DELETE access
2. Client sends SET_INFO with FileDispositionInformation (DeletePending=1)
3. Server marks file for deletion
4. On CLOSE, if DeletePending is set, actually delete the file

**Implementation steps:**
1. Add `DeletePending bool` field to `OpenFile` struct in `handler.go`
2. In SET_INFO handler, parse the 1-byte DeletePending flag
3. Set `openFile.DeletePending = true` and store back
4. In CLOSE handler, check if DeletePending is set
5. If set, call `metadataStore.DeleteFile()` or `metadataStore.DeleteDirectory()`
6. Handle errors (directory not empty, file in use, etc.)

**File:** `internal/protocol/smb/v2/handlers/close.go` - Add deletion logic

---

## Fix 3: Fix Timestamp Updates (FileBasicInformation)

**File:** `internal/protocol/smb/v2/handlers/set_info.go`

**Current code appears to handle FileBasicInformation but may have issues.**

**MS-FSCC 2.4.7 FILE_BASIC_INFORMATION structure (40 bytes):**
```
CreationTime (8 bytes)      - FILETIME
LastAccessTime (8 bytes)    - FILETIME
LastWriteTime (8 bytes)     - FILETIME
ChangeTime (8 bytes)        - FILETIME
FileAttributes (4 bytes)    - File attributes flags
Reserved (4 bytes)          - Padding
```

**Special values:**
- `0` = Don't change this timestamp
- `-1` (0xFFFFFFFFFFFFFFFF) = Don't change this timestamp
- `-2` (0xFFFFFFFFFFFFFFFE) = Set to current time

**Implementation steps:**
1. Review `DecodeFileBasicInfo()` in `encoding.go`
2. Ensure special values (0, -1, -2) are handled correctly
3. Verify `SMBTimesToSetAttrs()` correctly converts FILETIME to Go time
4. Check `metadataStore.SetFileAttributes()` is being called with correct values
5. Add debug logging to trace the actual values being set

---

## Fix 4: Fix Symlink Display in Directory Listings

**File:** `internal/protocol/smb/v2/handlers/query_directory.go`

**Problem:** Symlinks show as regular files (`.rwx------`) instead of symlinks (`lrwx------`)

**Root cause:** `FileAttrToSMBAttributes()` may not be setting `FILE_ATTRIBUTE_REPARSE_POINT` for symlinks.

**File:** `internal/protocol/smb/v2/handlers/conversions.go` (or wherever `FileAttrToSMBAttributes` is)

**Implementation steps:**
1. Find `FileAttrToSMBAttributes()` function
2. Add check: if `attr.Type == metadata.FileTypeSymlink`, set `FILE_ATTRIBUTE_REPARSE_POINT`
3. Verify the SMB types constant exists: `types.FileAttributeReparsePoint = 0x400`
4. Test with `ls -la` to verify symlinks show correctly

**MS-FSCC FileAttributes flags:**
```go
FileAttributeReparsePoint = 0x00000400  // Symlinks, mount points
```

---

## Fix 5: Implement Readlink (FSCTL_GET_REPARSE_POINT)

**File:** `internal/protocol/smb/v2/handlers/ioctl.go` (may need to create)

**SMB2 IOCTL command with FSCTL_GET_REPARSE_POINT (0x000900A8)**

**Implementation steps:**
1. Check if IOCTL handler exists
2. Add case for `FSCTL_GET_REPARSE_POINT`
3. Call `metadataStore.ReadSymlink(authCtx, handle)`
4. Encode response as REPARSE_DATA_BUFFER (MS-FSCC 2.1.2.1)
5. Return symlink target path

**REPARSE_DATA_BUFFER for symlinks:**
```
ReparseTag (4 bytes)           - IO_REPARSE_TAG_SYMLINK (0xA000000C)
ReparseDataLength (2 bytes)    - Length of data after header
Reserved (2 bytes)             - 0
SubstituteNameOffset (2 bytes) - Offset to substitute name
SubstituteNameLength (2 bytes) - Length of substitute name
PrintNameOffset (2 bytes)      - Offset to print name
PrintNameLength (2 bytes)      - Length of print name
Flags (4 bytes)                - 0 = absolute, 1 = relative
PathBuffer (variable)          - UTF-16LE encoded paths
```

---

## Testing Plan

After each fix, run these tests:

```bash
# Test 1: Rename
mv /tmp/smb/test_dir/file.txt /tmp/smb/test_dir/renamed.txt
ls -la /tmp/smb/test_dir/

# Test 2: Delete
rm /tmp/smb/test_dir/renamed.txt
ls -la /tmp/smb/test_dir/

# Test 3: Touch (timestamps)
touch /tmp/smb/test_dir/hello.txt
stat /tmp/smb/test_dir/hello.txt

# Test 4: Truncate
echo "new content" > /tmp/smb/test_dir/hello.txt
cat /tmp/smb/test_dir/hello.txt

# Test 5: Symlink display
ln -s /tmp/smb/test_dir/hello.txt /tmp/smb/test_dir/link.txt
ls -la /tmp/smb/test_dir/  # Should show lrwx------ for link.txt

# Test 6: Readlink
readlink /tmp/smb/test_dir/link.txt

# Test 7: Delete directory
mkdir /tmp/smb/test_dir/todelete
rmdir /tmp/smb/test_dir/todelete
```

---

## Priority Order

1. **Fix 2: Delete** - Most commonly needed, relatively simple
2. **Fix 1: Rename** - Very common, moderate complexity
3. **Fix 4: Symlink display** - Simple fix, improves UX
4. **Fix 3: Timestamps** - May already work, needs investigation
5. **Fix 5: Readlink** - Lower priority, needs IOCTL infrastructure

---

## References

- [MS-SMB2] SMB2 Protocol: https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/
- [MS-FSCC] File System Control Codes: https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/
- SET_INFO: MS-SMB2 2.2.39
- FileRenameInformation: MS-FSCC 2.4.34
- FileDispositionInformation: MS-FSCC 2.4.11
- FileBasicInformation: MS-FSCC 2.4.7
- REPARSE_DATA_BUFFER: MS-FSCC 2.1.2.1
