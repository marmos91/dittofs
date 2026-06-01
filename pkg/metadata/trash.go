package metadata

import (
	"fmt"
	"path"
)

// ValidateExcludePatterns reports the first malformed glob in patterns, or nil
// when every pattern is a valid path.Match glob. Excluded() is deliberately
// tolerant at match time (a bad glob simply never matches), so invalid patterns
// must be rejected at the point they are SET — otherwise a typo'd exclude
// (e.g. "[") silently never matches and the operator's intent is lost. The
// share create/update handlers call this before persisting TrashExcludePatterns.
func ValidateExcludePatterns(patterns []string) error {
	for _, pat := range patterns {
		// path.Match only ever returns ErrBadPattern; the probe target is
		// irrelevant — we just need Match to parse the pattern.
		if _, err := path.Match(pat, "probe"); err != nil {
			return fmt.Errorf("invalid exclude pattern %q: %w", pat, err)
		}
	}
	return nil
}

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
