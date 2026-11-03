// Package common provides helpers shared by every protocol adapter (NFSv3,
// NFSv4, SMB2/3).
//
// # Error mapping
//
// The metadata store error → protocol status translation lives in the
// per-adapter types packages (internal/adapter/nfs/types,
// internal/adapter/nfs/v4/types, internal/adapter/smb/types) as StatusFor
// / StatusForErr — the single source of truth per protocol, with a full
// enum-walk test per package. The SMB LOCK path uses StatusForLock /
// StatusForLockErr (lock-vs-general divergence, MS-SMB2 3.3.5.14).
//
// # File content
//
// No content store is wired (block.ErrNoContentStore). Every data operation
// answers ErrNoContentStore, which each protocol's StatusForErr maps to its
// generic I/O status.
//
// # Size changes
//
// A size change (NFS SETATTR and CREATE truncate, SMB end-of-file, allocation
// and overwrite) is a metadata update only while no content store is wired.
//
// decision: a shrink commits the new size and reaps no FileChunk row past the
// new EOF, because reaping them was the content store's truncate. The rows are
// stranded until the cutover that wires the engine through vfs.Service, which
// must replace this with the engine's truncate. The stranded-row reconcile
// (`dfsctl store block gc --reconcile`) reaps only the rows of a payload whose
// inode is gone, so it does not reclaim these; nothing reads them meanwhile,
// since every read answers ErrNoContentStore.
package common
