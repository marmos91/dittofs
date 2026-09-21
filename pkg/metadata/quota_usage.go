package metadata

import (
	"encoding/json"
	"fmt"
)

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

// MarshalJSON renders the scope as the same name the REST and CLI quota
// surfaces already use, so a scope reads the same whichever endpoint produced
// it rather than as a bare 0 or 1.
func (s QuotaScope) MarshalJSON() ([]byte, error) {
	if s != QuotaScopeUser && s != QuotaScopeGroup {
		return nil, fmt.Errorf("quota scope %d: not a scope", uint8(s))
	}
	return json.Marshal(s.String())
}

// UnmarshalJSON accepts the names MarshalJSON writes. An unknown name is an
// error rather than a silent QuotaScopeUser, which would attribute a group
// bucket to a uid.
func (s *QuotaScope) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	switch name {
	case "user":
		*s = QuotaScopeUser
	case "group":
		*s = QuotaScopeGroup
	default:
		return fmt.Errorf("quota scope %q: not a scope", name)
	}
	return nil
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
type UsageStat struct {
	// Bytes is the sum of logical sizes of regular files owned by the identity.
	Bytes int64
	// Files is the number of regular files owned by the identity (inode count).
	Files int64
}

// QuotaDrift reports one usage bucket whose maintained counter disagrees with
// the value derived from the store's file rows. Both numbers travel: an
// operator deciding whether to run a rebuild needs to see how far apart they
// are and in which direction, not just that they differ.
type QuotaDrift struct {
	// Share is the share the bucket belongs to.
	Share string `json:"share"`
	// Scope is whether the bucket is keyed by owning uid or owning gid.
	Scope QuotaScope `json:"scope"`
	// ID is the owning uid or gid.
	ID uint32 `json:"identity_id"`
	// Counter is what the maintained counter reports for the bucket.
	Counter UsageStat `json:"counter"`
	// Derived is what the store's file rows add up to for the bucket.
	Derived UsageStat `json:"derived"`
}
