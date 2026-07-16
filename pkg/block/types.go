package block

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BlockSize is the size of a single block (8MB). This is the single source of
// truth -- all packages should reference this constant instead of defining
// their own copies.
const BlockSize = 8 * 1024 * 1024

// HashSize is the size of content hashes (BLAKE3 = 32 bytes).
const HashSize = 32

// ContentHash represents a BLAKE3-256 content hash. Width is 32 bytes
// matching SHA-256 wire-compat so legacy metadata deserializes unchanged.
type ContentHash [HashSize]byte

// String returns the hex-encoded hash string.
func (h ContentHash) String() string {
	return hex.EncodeToString(h[:])
}

// CASKey returns the content-addressed key in scheme "blake3:{hex}".
// Used for the local CAS key format and the S3 x-amz-meta-content-hash header.
func (h ContentHash) CASKey() string {
	return "blake3:" + hex.EncodeToString(h[:])
}

// IsZero returns true if the hash is all zeros (uninitialized).
func (h ContentHash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// ParseContentHash parses a hex-encoded hash string.
func ParseContentHash(s string) (ContentHash, error) {
	var h ContentHash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	if len(b) != HashSize {
		return h, ErrInvalidHash
	}
	copy(h[:], b)
	return h, nil
}

// BlockState is the lifecycle state of a FileChunk: Pending -> Syncing -> Remote.
//
//   - Pending (0): RefCount >= 1, not yet uploaded. Safe zero value for legacy
//     rows deserialized without this field.
//   - Syncing (1): Claimed by a syncer goroutine; upload in flight.
//
// - Remote (2): PUT + metadata-txn confirmed; eligible for local eviction.
//
// Write-after-sync resets Remote -> Pending (clears Hash + BlockStoreKey).
type BlockState uint8

const (
	BlockStatePending BlockState = 0
	BlockStateSyncing BlockState = 1
	BlockStateRemote  BlockState = 2
)

// String returns the string representation of BlockState.
func (s BlockState) String() string {
	switch s {
	case BlockStatePending:
		return "Pending"
	case BlockStateSyncing:
		return "Syncing"
	case BlockStateRemote:
		return "Remote"
	default:
		return fmt.Sprintf("BlockState(%d)", s)
	}
}

// MarshalJSON encodes a ContentHash as the canonical CAS scheme string
// "blake3:{hex}" (mirrors CASKey()). Round-trips with UnmarshalJSON.
//
// Added in to drive ChunkRef JSON serialization.
// Without this, encoding/json would default to base64 for the [32]byte
// array — readable diffs in Postgres/Badger payloads would be impossible.
func (h ContentHash) MarshalJSON() ([]byte, error) {
	out := make([]byte, 0, 1+len("blake3:")+HashSize*2+1)
	out = append(out, '"')
	out = append(out, h.CASKey()...)
	out = append(out, '"')
	return out, nil
}

// UnmarshalJSON accepts the canonical "blake3:{hex}" form, the bare
// "{hex}" form, and the pre-Phase-12 default base64 form (encoding/json's
// default for [32]byte arrays without a custom MarshalJSON). The base64
// fallback preserves backward compatibility for FileChunk rows persisted
// by Badger before this MarshalJSON existed.
func (h *ContentHash) UnmarshalJSON(data []byte) error {
	// v0.14.x and earlier had no custom MarshalJSON — encoding/json
	// serialized [32]byte as a JSON number array: [0,0,...,0]. Accept
	// that form so develop can read legacy badger metadata.
	if len(data) > 0 && data[0] == '[' {
		var arr [HashSize]byte
		if err := json.Unmarshal(data, &arr); err == nil {
			*h = ContentHash(arr)
			return nil
		}
		return fmt.Errorf("ContentHash.UnmarshalJSON: invalid JSON array: %q", data)
	}
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("ContentHash.UnmarshalJSON: not a JSON string: %q", data)
	}
	s := string(data[1 : len(data)-1])
	// Canonical / hex form.
	hexStr := strings.TrimPrefix(s, "blake3:")
	if len(hexStr) == HashSize*2 {
		parsed, err := ParseContentHash(hexStr)
		if err == nil {
			*h = parsed
			return nil
		}
	}
	// Legacy: encoding/json's default base64 form for [32]byte (no custom
	// MarshalJSON existed before). Decode and copy.
	b, b64Err := base64.StdEncoding.DecodeString(s)
	if b64Err == nil && len(b) == HashSize {
		copy(h[:], b)
		return nil
	}
	return fmt.Errorf("ContentHash.UnmarshalJSON: %w (input %q)", ErrInvalidHash, s)
}

// ChunkRef is a single content-addressed reference to a chunk of a
// file's payload. The list FileAttr.Blocks []ChunkRef is sorted by
// Offset and covers the file end-to-end (gaps within Size are sparse
// holes, zero-filled on read per).
//
// Hash is the BLAKE3 content hash identifying the chunk.
// Offset is the byte offset within the file (uint64 to support files
// >4 GiB; VM workload requirement).
// Size is the chunk length in bytes (FastCDC min 1 MiB, max 16 MiB
// uint32 chosen to match FileChunk.DataSize column type).
//
// See, decisions.
type ChunkRef struct {
	Hash   ContentHash `json:"hash"`
	Offset uint64      `json:"offset"`
	Size   uint32      `json:"size"`
}

// MergeChunkRefsByOffset overlays incoming block refs onto existing ones,
// keyed by byte range. Every incoming ref is kept; an existing ref is kept
// only if its [Offset, Offset+Size) range does not overlap any incoming ref.
// The result is sorted by Offset ascending.
//
// This models a rollup pass committing a slice of a file: appended chunks
// (new, non-overlapping offsets) extend the list, while an in-place rewrite
// (incoming chunks covering an existing byte range, possibly with different
// FastCDC boundaries) replaces the overlapped existing chunks. It is the
// accumulation step that keeps FileAttr.Blocks complete across multi-pass
// rollups instead of replacing it with only the latest pass (#789).
func MergeChunkRefsByOffset(existing, incoming []ChunkRef) []ChunkRef {
	if len(incoming) == 0 {
		out := make([]ChunkRef, len(existing))
		copy(out, existing)
		sortChunkRefsByOffset(out)
		return out
	}
	out := make([]ChunkRef, 0, len(existing)+len(incoming))
	for _, e := range existing {
		eEnd := e.Offset + uint64(e.Size)
		overlaps := false
		for _, in := range incoming {
			inEnd := in.Offset + uint64(in.Size)
			if e.Offset < inEnd && in.Offset < eEnd {
				overlaps = true
				break
			}
		}
		if !overlaps {
			out = append(out, e)
		}
	}
	out = append(out, incoming...)
	sortChunkRefsByOffset(out)
	return out
}

func sortChunkRefsByOffset(b []ChunkRef) {
	sort.Slice(b, func(i, j int) bool { return b[i].Offset < b[j].Offset })
}

// PruneChunkRefsToSize drops block refs that lie entirely at or beyond size,
// so the list never over-references content past EOF. A ref that straddles
// the new EOF (Offset < size <= Offset+Size) is kept intact — block payloads
// are content-addressed and immutable, so the tail bytes past EOF are simply
// ignored on read; only fully-past-EOF refs are removed. The input slice is
// not mutated; the result is sorted by Offset ascending.
//
// This is the truncate counterpart to MergeChunkRefsByOffset: a size-down
// SetAttr must trim FileAttr.Blocks the same way a rewrite would, otherwise
// stale-tail refs survive, the GC holds extra blocks, and a restore would
// emit a file longer than the current size.
func PruneChunkRefsToSize(refs []ChunkRef, size uint64) []ChunkRef {
	out := make([]ChunkRef, 0, len(refs))
	for _, r := range refs {
		if r.Offset < size {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sortChunkRefsByOffset(out)
	return out
}

// FileChunk is the single chunk entity in DittoFS — the metadata manifest
// entry for one content-defined (FastCDC+BLAKE3) chunk. Content-addressed
// chunks with the same hash are shared across files for dedup.
//
// Lifecycle
// 1. Pending — created on write (BlockStoreKey empty).
// 2. Syncing — claim batch flipped State and stamped LastSyncAttemptAt.
// 3. Remote — PUT + metadata-txn confirmed (BlockStoreKey set).
type FileChunk struct {
	// ID is a stable UUID for this chunk.
	ID string

	// Hash is the BLAKE3-256 of the chunk data. Zero value means pending/incomplete.
	Hash ContentHash

	// DataSize is the actual bytes written in this chunk.
	DataSize uint32

	// BlockStoreKey is the opaque key in the remote block store (S3 key, FS path, etc.).
	// Empty means not synced to remote.
	BlockStoreKey string

	// RefCount is the number of files referencing this chunk.
	RefCount uint32

	// LastAccess is used for LRU eviction.
	LastAccess time.Time

	// LastSyncAttemptAt is the time the syncer last claimed this chunk.
	// The restart-recovery janitor requeues Syncing rows whose attempt
	// exceeds syncer.claim_timeout. Zero value means never attempted.
	LastSyncAttemptAt time.Time `json:"last_sync_attempt_at,omitempty"`

	// CreatedAt is when the chunk was created.
	CreatedAt time.Time

	// State is the chunk lifecycle state. Zero value (Pending) is the safe
	// default for legacy chunks.
	State BlockState `json:"state"`
}

// IsRemote returns true if the chunk has been synced to the remote block store.
// Dual-read fallback: legacy zero-valued rows (State==Pending) that
// already carry a BlockStoreKey were uploaded under the legacy non-CAS path
// and must still be treated as Remote during the dual-read window.
func (b *FileChunk) IsRemote() bool {
	if b.State == BlockStateRemote {
		return true
	}
	return b.State == BlockStatePending && b.BlockStoreKey != ""
}

// IsFinalized returns true if the chunk's upload is complete (State==Remote).
func (b *FileChunk) IsFinalized() bool {
	return b.State == BlockStateRemote
}

// ParseBlockID extracts the payloadID and block index from an internal
// blockID string. BlockID format: "{payloadID}/{blockIdx}" where payloadID
// may itself contain '/' characters (only the LAST '/' separates the index).
//
// Example: "export/docs/report.pdf/7" -> ("export/docs/report.pdf", 7, nil).
//
// Returns a wrapped error for malformed inputs: missing separator
// empty trailing index, or non-numeric index. Callers that previously
// relied on silent zero-value returns must now propagate the error.
func ParseBlockID(blockID string) (payloadID string, blockIdx uint64, err error) {
	lastSlash := strings.LastIndex(blockID, "/")
	if lastSlash <= 0 {
		return "", 0, fmt.Errorf("parse blockID %q: missing payloadID/idx separator: %w", blockID, ErrInvalidPayloadID)
	}
	payloadID = blockID[:lastSlash]
	idxStr := blockID[lastSlash+1:]
	if idxStr == "" {
		return "", 0, fmt.Errorf("parse blockID %q: empty block index: %w", blockID, ErrInvalidPayloadID)
	}
	blockIdx, parseErr := strconv.ParseUint(idxStr, 10, 64)
	if parseErr != nil {
		return "", 0, fmt.Errorf("parse blockID %q: invalid block index: %w", blockID, parseErr)
	}
	return payloadID, blockIdx, nil
}
