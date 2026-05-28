package snapshot

import (
	"errors"
)

// ErrInvalidManifestLine is returned by ReadManifest when a line in the
// manifest cannot be parsed as a 32-byte hex ContentHash. Callers should
// match it with errors.Is. The wrapped error carries the offending line
// number and the underlying parse error.
var ErrInvalidManifestLine = errors.New("snapshot: invalid manifest line")
