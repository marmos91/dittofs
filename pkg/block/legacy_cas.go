// Package block — legacy_cas.go.
//
// Migration-only helpers for the legacy standalone-CAS object layout
// (one sealed chunk per remote object under "cas/"). The production
// write/read paths no longer use this layout (#1493); the only consumers
// are the remote backends' legacy_cas_migration.go accessors feeding the
// one-shot cas→blocks startup migration. Delete this file with them when
// the migration is retired.
package block

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrCASKeyMalformed is returned by ParseCASKey for any input that does
// not match the cas/{hh}/{hh}/{hex} shape.
var ErrCASKeyMalformed = errors.New("blockstore: malformed CAS key")

// FormatCASKey returns the flat S3 object key for a content-addressed block.
// Format: "cas/{hex[0:2]}/{hex[2:4]}/{hex}". Two-level fanout caps the
// top-level prefix count at 256 and bounds per-prefix file count predictably.
// Mirror to ParseCASKey.
func FormatCASKey(h ContentHash) string {
	hexStr := hex.EncodeToString(h[:])
	return "cas/" + hexStr[0:2] + "/" + hexStr[2:4] + "/" + hexStr
}

// ParseCASKey parses an S3 object key produced by FormatCASKey and returns
// the embedded ContentHash. Returns ErrCASKeyMalformed wrapped with the
// offending input on any shape, length, or hex error.
// Symmetric to FormatCASKey.
func ParseCASKey(key string) (ContentHash, error) {
	const prefix = "cas/"
	if !strings.HasPrefix(key, prefix) {
		return ContentHash{}, fmt.Errorf("%w: missing %q prefix in %q", ErrCASKeyMalformed, prefix, key)
	}
	rest := key[len(prefix):]
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return ContentHash{}, fmt.Errorf("%w: expected 3 segments after prefix in %q", ErrCASKeyMalformed, key)
	}
	shard1, shard2, hexStr := parts[0], parts[1], parts[2]
	if len(shard1) != 2 || len(shard2) != 2 {
		return ContentHash{}, fmt.Errorf("%w: shard segments must be 2 hex chars in %q", ErrCASKeyMalformed, key)
	}
	if len(hexStr) != HashSize*2 {
		return ContentHash{}, fmt.Errorf("%w: hex hash must be %d chars in %q", ErrCASKeyMalformed, HashSize*2, key)
	}
	if hexStr[0:2] != shard1 || hexStr[2:4] != shard2 {
		return ContentHash{}, fmt.Errorf("%w: shard prefix does not match hash in %q", ErrCASKeyMalformed, key)
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return ContentHash{}, fmt.Errorf("%w: %v", ErrCASKeyMalformed, err)
	}
	var h ContentHash
	copy(h[:], raw)
	return h, nil
}
