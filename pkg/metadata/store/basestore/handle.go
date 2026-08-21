package basestore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// GenerateHandle mints a fresh UUID-based file handle for the named share.
//
// The path a caller passes to the store-level GenerateHandle is ignored: every
// backend addresses files by UUID, and the path is carried in the file record.
// Returns an error rather than panicking when the encoded handle would exceed
// the 64-byte limit.
func GenerateHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return metadata.GenerateNewHandle(shareName)
}

// ShareOfHandle returns the share a handle belongs to, or the empty string when
// the handle does not decode.
//
// Usage and statistics are per-share, so every backend needs the handle's share
// to answer statfs. An undecodable handle is not an error there, but what the
// empty string means is the CALLER's policy, not this function's — the two in
// tree differ:
//
//   - The KV backends treat it as a share name that matches nothing, so usage
//     reads back zero. A share with no files and a share that does not exist are
//     indistinguishable by usage anyway.
//   - The SQL backends' statfsQuery treats it as a sentinel for "no share
//     predicate" and falls back to the store-wide aggregate, which keeps a
//     single-share deployment reporting the same numbers it always has.
//
// Decide which you want at the call site rather than assuming empty means zero.
func ShareOfHandle(handle metadata.FileHandle) string {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return ""
	}
	return shareName
}
