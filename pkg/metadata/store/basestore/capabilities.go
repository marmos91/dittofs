package basestore

import "github.com/marmos91/dittofs/pkg/metadata"

// DefaultCapabilities returns the filesystem capabilities a backend reports
// when the operator has not configured any: 1 MiB transfers, POSIX-shaped
// name and path limits, and no hard file-size ceiling.
//
// The 1 MiB preferred read/write matches the Linux knfsd default and keeps NFS
// round-trips per block down. Capabilities are advertised to clients, not
// enforced here, so a backend that genuinely cannot do something (ACLs today)
// must say so rather than inherit a true from this set.
func DefaultCapabilities() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
		// Transfer sizes.
		MaxReadSize:        1048576,
		PreferredReadSize:  1048576,
		MaxWriteSize:       1048576,
		PreferredWriteSize: 1048576,

		// Limits.
		MaxFileSize:      9223372036854775807, // 2^63-1, practically unlimited
		MaxFilenameLen:   255,                 // standard Unix limit
		MaxPathLen:       4096,                // standard Unix limit
		MaxHardLinkCount: 32767,               // similar to ext4

		// Features.
		SupportsHardLinks:     true,  // link counts are tracked
		SupportsSymlinks:      true,  // symlink targets are stored
		CaseSensitive:         true,  // keys are case-sensitive
		CasePreserving:        true,  // exact filenames are stored
		ChownRestricted:       false, // chown is allowed
		SupportsACLs:          false, // no ACL support yet
		SupportsExtendedAttrs: true,  // EAs persist on FileAttr.EAs
		TruncatesLongNames:    true,  // reject with an error, do not truncate

		// Time resolution.
		TimestampResolution: 1, // 1ns, Go time.Time precision
	}
}
