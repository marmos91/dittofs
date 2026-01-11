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

## Store Implementation Notes

### Memory Store
- UUID handles - unstable across restarts (testing only)
- Single RWMutex - all operations block each other

### BadgerDB Store
- Path-based handles - stable across restarts
- Stats cached with TTL (avoid expensive scans for macOS Finder's frequent FSSTAT)
- Production defaults: 1GB block cache, 512MB index cache

### PostgreSQL Store
- UUID handles with shareName encoding for distributed deployments
- Designed for multi-node scenarios

## Common Mistakes

1. **Parsing handles** - Router extracts share name, that's it
2. **Locking across transactions** - Will deadlock
3. **Assuming memory store persistence** - It's ephemeral
4. **Forgetting lock cleanup** - Files deleted, locks remain
