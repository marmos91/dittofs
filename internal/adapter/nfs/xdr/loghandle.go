package xdr

import (
	"encoding/hex"
	"log/slog"
)

// LazyHandle wraps a raw file handle and defers its "0x…" hex rendering to
// log-emit time via slog.LogValuer. Handlers build one per op on the READ /
// WRITE / COMMIT hot path, where most ops never emit at their log level, so the
// formatting and its allocation are skipped. It renders like
// fmt.Sprintf("0x%x", handle).
type LazyHandle []byte

// LogValue implements slog.LogValuer.
func (h LazyHandle) LogValue() slog.Value {
	return slog.StringValue("0x" + hex.EncodeToString(h))
}
