package metadata

import "path"

// RecycleDirName is the reserved per-share recycle-bin directory (Synology
// convention). Created lazily at the share root when trash is enabled.
const RecycleDirName = "#recycle"

// TrashConfig is a per-share recycle-bin policy snapshot, returned by a
// TrashPolicy under lock so callers never read a shared, mutating pointer.
type TrashConfig struct {
	Enabled         bool
	ExcludePatterns []string
}

// Excluded reports whether a base name matches any exclude glob and should
// therefore bypass the bin (immediate delete).
func (c TrashConfig) Excluded(name string) bool {
	for _, pat := range c.ExcludePatterns {
		if ok, err := path.Match(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

// TrashPolicy yields the recycle-bin config for the share owning a handle.
// Implemented by the runtime shares service with a locked accessor; nil on a
// MetadataService means trash is globally disabled (delete behaves as before).
type TrashPolicy interface {
	// TrashConfigForShare returns the policy for the named share. ok=false
	// when the share is unknown.
	TrashConfigForShare(shareName string) (cfg TrashConfig, ok bool)
}
