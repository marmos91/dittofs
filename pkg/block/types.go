// Package block is the shared vocabulary of the block layer: content hashes,
// chunk and manifest types, block state, the error sentinels and the on-disk
// format-version convention. It is a leaf — it imports nothing else in this
// repository. See README.md.
package block

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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
// Write-after-sync resets Remote -> Pending (clears Hash).
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
// It exists to drive ChunkRef JSON serialization: without it,
// encoding/json would serialize the [32]byte array as a JSON number
// array — readable diffs in Postgres/Badger payloads would be impossible.
func (h ContentHash) MarshalJSON() ([]byte, error) {
	out := make([]byte, 0, 1+len("blake3:")+HashSize*2+1)
	out = append(out, '"')
	out = append(out, h.CASKey()...)
	out = append(out, '"')
	return out, nil
}

// UnmarshalJSON accepts the canonical "blake3:{hex}" form written by
// MarshalJSON, the bare "{hex}" form, and the JSON number array that
// encoding/json emits for a bare [32]byte.
//
// decision: the number-array branch is read-compat for badger stores written
// before this type had a MarshalJSON, and it stays even though nothing writes
// that form any more. Refusing it does not fail loudly: the row-listing scan
// skips any fb: row whose value does not unmarshal, so a refused hash drops the
// row with a nil error rather than surfacing one, and a caller reading extents
// off that list then reports the range as a hole instead of erroring — stored
// bytes read back as zeros. Withdraw it only once no store can hold the form
// AND the listing path distinguishes a decode failure from an absent row.
func (h *ContentHash) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '[' {
		// Decode the elements into pointers, because a decode error does not
		// mean the input was well formed. Unmarshalling into [HashSize]byte has
		// three silent paths: a short array is zero-filled, a long array's tail
		// is dropped, and a null element is a no-op that leaves its byte zero —
		// none of them an error, all of them yielding a confidently wrong hash.
		// A pointer element makes absence representable, and the same pass
		// carries the element count and rejects a wrong type or an
		// out-of-byte-range value, so this is the whole validation rather than
		// one guard stacked on another.
		var elems []*uint8
		if err := json.Unmarshal(data, &elems); err != nil {
			// Wrap rather than replace: the decoder's own message names the
			// offending value and the type it would not fit, which is the
			// detail that tells a corrupt legacy row apart from a merely
			// unexpected one.
			return fmt.Errorf("ContentHash.UnmarshalJSON: invalid JSON array %q: %w", data, err)
		}
		if len(elems) != HashSize {
			return fmt.Errorf("ContentHash.UnmarshalJSON: %w: JSON array has %d elements, want %d",
				ErrInvalidHash, len(elems), HashSize)
		}
		var parsed ContentHash
		for i, e := range elems {
			if e == nil {
				return fmt.Errorf("ContentHash.UnmarshalJSON: %w: JSON array element %d is null",
					ErrInvalidHash, i)
			}
			parsed[i] = *e
		}
		*h = parsed
		return nil
	}
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("ContentHash.UnmarshalJSON: not a JSON string: %q", data)
	}
	s := string(data[1 : len(data)-1])
	hexStr := strings.TrimPrefix(s, "blake3:")
	if len(hexStr) == HashSize*2 {
		parsed, err := ParseContentHash(hexStr)
		if err == nil {
			*h = parsed
			return nil
		}
	}
	return fmt.Errorf("ContentHash.UnmarshalJSON: %w (input %q)", ErrInvalidHash, s)
}

// ChunkRef is a single content-addressed reference to a chunk of a
// file's payload. The list FileAttr.Blocks []ChunkRef is sorted by
// Offset and covers the file end-to-end (gaps within Size are sparse
// holes, zero-filled on read).
//
// Hash is the BLAKE3 content hash identifying the chunk.
// Offset is the byte offset within the file (uint64 to support files
// >4 GiB; VM workload requirement).
// Size is the chunk length in bytes (FastCDC min 1 MiB, max 16 MiB
// uint32 chosen to match FileChunk.DataSize column type).
// StartOffset is where inside the chunk the referenced bytes begin, mirroring
// FileChunk.StartOffset; it must travel with the ref because the projection is
// what a clone and a manifest repair rebuild rows from, and a ref that dropped
// it would rebuild a row serving the chunk's head at Offset.
type ChunkRef struct {
	Hash        ContentHash `json:"hash"`
	Offset      uint64      `json:"offset"`
	Size        uint32      `json:"size"`
	StartOffset uint32      `json:"start_offset,omitempty"`
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
// A size-down SetAttr must trim FileAttr.Blocks the same way a rewrite
// would, otherwise stale-tail refs survive, the GC holds extra blocks, and
// a restore would emit a file longer than the current size.
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
// 1. Pending — created on write.
// 2. Syncing — claim batch flipped State and stamped LastSyncAttemptAt.
// 3. Remote — PUT + metadata-txn confirmed.
type FileChunk struct {
	// ID is a stable UUID for this chunk.
	ID string

	// Hash is the BLAKE3-256 of the chunk data. Zero value means pending/incomplete.
	Hash ContentHash

	// DataSize is how many of the chunk's bytes belong to this file at this
	// row's offset — the row's claim, which is what coverage lookups and a
	// cold-read hydrate honour. It is the chunk's full length except where a
	// narrow cut the row down to the stretch that survived: the chunk on the
	// remote still holds all of its bytes and is still hash-verified over all of
	// them, the row just stops claiming what it gave up.
	DataSize uint32

	// StartOffset is where inside the chunk the claimed bytes begin, so the row
	// covers file bytes [rowOffset, rowOffset+DataSize) with chunk bytes
	// [StartOffset, StartOffset+DataSize). Zero for every row a carve writes and
	// for every row narrowed only off its tail, which is why a row written before
	// the field existed means exactly what it meant then: the claim starts at the
	// chunk's first byte.
	//
	// A non-zero value comes from narrowing a row off its HEAD, which is what a
	// carve span ending inside a row that also starts inside it needs: the span
	// re-chunked that row's head and nothing else, so the row keeps only what
	// lies past the span. The row's ID moves to the first byte it still claims,
	// so every coverage and succession lookup goes on reading a row's start
	// straight off its ID; only the paths that read the chunk's own bytes need
	// this offset.
	StartOffset uint32

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
func (b *FileChunk) IsRemote() bool {
	return b.State == BlockStateRemote
}
