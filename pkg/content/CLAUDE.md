# pkg/content

Content service layer - handles raw file bytes, coordinates caches and stores.

## Architecture

```
ContentServiceInterface (interface.go)
         ↓
ContentService (service.go) - coordinates store + cache
         ↓
ContentStore (store.go) - base interface
    + ReadAtContentStore (optional)
    + IncrementalWriteStore (optional)
         ↓
store/{memory,fs,s3}/ - implementations
```

## Critical Conventions

### ContentID Is Opaque
- Format varies: UUID (memory/fs), S3 key path (s3), SHA256 (content-addressable)
- Never parse or construct ContentIDs - let stores generate them

### Store Knows Only Bytes
Content stores don't know about:
- File metadata (attributes, permissions)
- Directory hierarchy
- Access control
- File handles

### Optional Interface Pattern
```go
// Check at runtime, fall back gracefully
if ras, ok := store.(ReadAtContentStore); ok {
    return ras.ReadAt(...)  // Efficient partial read
}
// Fall back to sequential read
```

## Store Implementation Notes

### S3 Store - Most Complex
- **Path-based keys**: `export/path/to/file` - mirrors filesystem for disaster recovery
- **Buffered deletion**: `Delete()` returns nil but may not complete immediately
- **Call `Close()` before shutdown** to flush pending deletions
- Range reads via HTTP Range header (~100x faster for small reads of large files)
- Multipart: 5MB min part size, max 10,000 parts

### Filesystem Store
- Random access via `ReadAt`/`WriteAt`
- Sparse file support via seeking
- UUIDs as filenames in configured directory

## Common Mistakes

1. **Concurrent writes to same ContentID** - undefined behavior (last-write-wins, corruption, or error)
2. **Assuming S3 Delete is synchronous** - it's batched async
3. **Not checking optional interfaces** - fall back to base behavior
4. **Writing beyond file size** - fills gap with zeros (sparse behavior), not an error
