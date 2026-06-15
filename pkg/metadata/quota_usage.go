package metadata

// QuotaScope identifies whether a per-identity usage counter or limit is keyed
// by owning user (uid) or owning group (gid). It is protocol-agnostic: NFS and
// SMB both map their callers to a POSIX-style uid/gid identity before reaching
// the metadata store.
type QuotaScope uint8

const (
	// QuotaScopeUser keys usage/limits by the file owner's uid (FileAttr.UID).
	QuotaScopeUser QuotaScope = iota
	// QuotaScopeGroup keys usage/limits by the file owner's gid (FileAttr.GID).
	QuotaScopeGroup
)

// String renders the scope for storage and logging. The values must stay
// stable: they are persisted by the postgres/badger backends and used as the
// REST/CLI scope identifier.
func (s QuotaScope) String() string {
	switch s {
	case QuotaScopeUser:
		return "user"
	case QuotaScopeGroup:
		return "group"
	default:
		return "unknown"
	}
}

// UsageStat is the per-identity accounting pair tracked by every metadata
// backend: total logical bytes and file (inode) count for regular files owned
// by a single uid or gid. Mirrors the store-wide GetUsedBytes counter but keyed
// by owner identity so per-user/per-group quotas can be enforced and reported.
//
// Only regular files contribute (directories, symlinks, devices do not), matching
// the store-wide usedBytes semantics. Files is the inode count used for the
// inode-quota dimension.
type UsageStat struct {
	// Bytes is the sum of logical sizes of regular files owned by the identity.
	Bytes int64
	// Files is the number of regular files owned by the identity (inode count).
	Files int64
}
