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
// by a single uid or gid within one share. Mirrors GetUsedBytesForShare but
// keyed by owner identity so per-user/per-group quotas can be enforced and
// reported.
//
// Only regular files contribute (directories, symlinks, devices do not),
// matching the per-share usage semantics. Files is the inode count used for the
// inode-quota dimension.
// The JSON names are lower case because this type reaches the REST surface
// inside a drift report, where every neighbouring field is lower case.
type UsageStat struct {
	// Bytes is the sum of logical sizes of regular files owned by the identity.
	Bytes int64 `json:"bytes"`
	// Files is the number of regular files owned by the identity (inode count).
	Files int64 `json:"files"`
}

// QuotaDrift reports one usage bucket whose maintained counter disagrees with
// the value derived from the store's file rows. Both numbers travel: an
// operator deciding whether to run a rebuild needs to see how far apart they
// are and in which direction, not just that they differ.
type QuotaDrift struct {
	// Share is the share the bucket belongs to.
	Share string `json:"share"`
	// Scope names whether the bucket is keyed by owning uid ("user"), owning gid
	// ("group"), or is the share's own total ("share"), rather than carrying the
	// numeric QuotaScope. The numeric values are a storage detail of the
	// backends, and a byte a corrupt key decoded to has to render as a row in
	// this report rather than fail the whole response — the report is what a
	// corrupt store is diagnosed with. A share row leaves ID at zero.
	Scope string `json:"scope"`
	// ID is the owning uid or gid.
	ID uint32 `json:"identity_id"`
	// Counter is what the maintained counter reports for the bucket.
	Counter UsageStat `json:"counter"`
	// Derived is what the store's file rows add up to for the bucket.
	Derived UsageStat `json:"derived"`
}
