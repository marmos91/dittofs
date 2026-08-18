package handlers

import (
	"encoding/hex"
	"log/slog"
)

// lazyFileID wraps an SMB2 file identifier and defers its hex rendering to
// log-emit time via slog.LogValuer. The READ and WRITE handlers build one per
// request, where most requests never emit at Debug level, so the formatting
// and its allocation are skipped entirely. It renders exactly like
// fmt.Sprintf("%x", id).
type lazyFileID [16]byte

// LogValue implements slog.LogValuer so the hex string is built only when a
// record is actually emitted (skipped for filtered-out levels).
func (id lazyFileID) LogValue() slog.Value {
	return slog.StringValue(hex.EncodeToString(id[:]))
}
