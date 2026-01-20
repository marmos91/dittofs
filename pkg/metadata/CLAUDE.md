# pkg/metadata

Metadata service layer - routes operations to correct stores based on share name.

## Architecture

```
MetadataServiceInterface (interface.go)
         ↓
MetadataService (service.go) - routes by share name
         ↓
MetadataStore (store.go) - interface
         ↓
store/{memory,badger,postgres}/ - implementations
```

## Critical Conventions

### File Handles Are Opaque
- Never parse handle contents - use `DecodeFileHandle()` for routing only
- Handle format varies: memory=UUID, badger=path-based, postgres=shareName+UUID
- Path-based handles enable metadata recovery from content store

### Lock Manager Ownership
- `MetadataService` owns lock managers (ephemeral, per-share)
- Locks do NOT survive server restart - they're advisory only
- Clean up locks when deleting files or orphaned locks persist

### Transaction Rules
- All stores support `WithTransaction(ctx, fn)` for atomic ops
- **Nested transactions NOT supported** - don't call `WithTransaction` inside a transaction
- Transaction isolation is store-specific (memory=mutex, badger=serializable)

## Content-Addressed Storage (ObjectStore)

The `ObjectStore` interface enables deduplication via content-addressed storage:

### Three-Level Hierarchy
```
Object (SHA-256 hash of file content)
   └─► Chunks (64MB logical segments, hash = SHA-256 of block hashes)
          └─► Blocks (4MB physical storage units, hash = SHA-256 of data)
```

### Key Types
- `ContentHash` - 32-byte SHA-256 hash identifying objects, chunks, and blocks
- `Object` - File content metadata (size, ref count, finalized flag)
- `ObjectChunk` - Chunk within an object (index, hash, block count)
- `ObjectBlock` - Block within a chunk (index, hash, upload status)

### Reference Counting
- All entities (Object, Chunk, Block) have `RefCount` fields
- `IncrementRefCount()` / `DecrementRefCount()` are atomic operations
- When RefCount reaches 0, entity is safe for garbage collection
- Hard links share the same Object (increments RefCount)

### Deduplication Flow
```
Write block → Hash(data) → FindBlockByHash()
              ↓ exists?
         Yes: IncrementBlockRefCount() + skip upload
         No:  PutBlock() + upload to block store
```

### FileAttr.ObjectID
- Files have an `ObjectID` field (ContentHash) linking to their Object
- Zero value = object not finalized or file has no content
- Multiple files can share the same ObjectID (hard links)

## Store Implementation Notes

### Memory Store
- UUID handles - unstable across restarts (testing only)
- Single RWMutex - all operations block each other
- ObjectStore data stored in memory (lost on restart)

### BadgerDB Store
- Path-based handles - stable across restarts
- Stats cached with TTL (avoid expensive scans for macOS Finder's frequent FSSTAT)
- Production defaults: 1GB block cache, 512MB index cache
- ObjectStore uses JSON-encoded values with key prefixes: `obj:`, `chunk:`, `block:`

### PostgreSQL Store
- UUID handles with shareName encoding for distributed deployments
- Designed for multi-node scenarios
- ObjectStore uses dedicated tables: `objects`, `object_chunks`, `object_blocks`

## Common Mistakes

1. **Parsing handles** - Router extracts share name, that's it
2. **Locking across transactions** - Will deadlock
3. **Assuming memory store persistence** - It's ephemeral
4. **Forgetting lock cleanup** - Files deleted, locks remain
5. **Decrementing RefCount without checking** - Always check if RefCount == 0 before deleting
6. **Deleting Objects before Chunks/Blocks** - Delete bottom-up (blocks → chunks → objects)
7. **Not using FindBlockByHash for dedup** - Always check before uploading new blocks
