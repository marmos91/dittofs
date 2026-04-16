package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Backup / Restore — Phase 2 ENG-01
// ============================================================================
//
// Wire format (D-03, D-09). Any structural change bumps badgerFormatVersion:
//
//	┌────────────────────────────────────────────────────────────────────┐
//	│ header_len: uint32 BE                                              │
//	│ header:     JSON (badgerBackupHeader)                              │
//	│ frames:     repeated {                                             │
//	│                 prefix_idx: uint8 (0..254; 0xFF reserved as EOF)   │
//	│                 key_len:    uint32 BE (bounded ≤ 1 MiB)            │
//	│                 key:        [key_len]byte                          │
//	│                 value_len:  uint32 BE (bounded ≤ 1 GiB)            │
//	│                 value:      [value_len]byte                        │
//	│             }                                                      │
//	│ eof_marker: uint8 = 0xFF                                           │
//	│ trailer:    uint32 BE CRC-32/IEEE of all frame bytes               │
//	└────────────────────────────────────────────────────────────────────┘
//
// The EOF marker is placed before the CRC trailer so the decoder can
// disambiguate it from a prefix_idx with a single byte-compare (no prefix
// index ever equals 0xFF because allBackupPrefixes has fewer than 255
// entries). The CRC is computed over every byte in the frames region —
// specifically the sequence of {prefix_idx, key_len, value_len, key, value}
// tuples — and is checked after the EOF marker has been consumed.
//
// Invariants enforced by Backup:
//
//   - The PayloadIDSet scan (over prefixFile) and the key/value emission loop
//     execute inside a single s.db.View(...) closure. This is how we honour
//     D-02 (same-snapshot PayloadIDSet) while simultaneously honouring D-03
//     (no call to badger's DB.Backup wrapper, which opens a distinct internal
//     read-timestamp we cannot share).
//
//   - All prefixes declared in allBackupPrefixes are streamed in a stable
//     order. The index of each prefix in that slice is the prefix_idx byte
//     written into every frame; the archive header records the same slice so
//     a future binary can reject archives referencing unknown prefixes.
//
// Invariants enforced by Restore:
//
//   - Destination store MUST be empty. Detected by probing prefixFile under a
//     read-only txn. Any existing key with prefix "f:" rejects the request
//     with metadata.ErrRestoreDestinationNotEmpty (D-06).
//
//   - Archive header's format_version must be ≤ the current binary's; every
//     prefix named in key_prefix_list must be known to this binary; otherwise
//     metadata.ErrRestoreCorrupt (D-09).
//
//   - CRC-32 trailer must validate. Any single-byte flip in a frame body is
//     surfaced as metadata.ErrRestoreCorrupt. Truncated archives (no EOF
//     marker, no trailer) are surfaced the same way.

const (
	// badgerFormatVersion is the per-engine wire-format version (D-09). Bump
	// this whenever the framing or header schema changes in a way that is not
	// backward-compatible via optional JSON fields.
	badgerFormatVersion = uint32(1)

	// badgerEOFMarker is written as the final byte of a well-formed archive.
	// No prefix_idx value ever collides with it because allBackupPrefixes has
	// far fewer than 255 entries and the restorer rejects out-of-range
	// prefix_idx values.
	badgerEOFMarker = uint8(0xFF)

	// badgerMaxKeyLen and badgerMaxValueLen cap the per-frame allocation we
	// accept during Restore. A malicious archive cannot force us to allocate
	// more than this per frame; see T-02-03-04 in the plan threat model.
	badgerMaxKeyLen   = uint32(1 << 20) // 1 MiB
	badgerMaxValueLen = uint32(1 << 30) // 1 GiB

	// badgerMaxHeaderLen bounds the JSON header. Header carries only the
	// prefix list and format metadata; 64 KiB is far above anything legitimate.
	badgerMaxHeaderLen = uint32(1 << 16)
)

// badgerBackupHeader is JSON-encoded as the first entry in the archive
// (D-09). New fields MUST be added with `omitempty` to stay compatible with
// archives produced by older binaries in the same major version.
type badgerBackupHeader struct {
	FormatVersion uint32   `json:"format_version"`
	BadgerVersion string   `json:"badger_version"`
	KeyPrefixList []string `json:"key_prefix_list"`
	CreatedAt     string   `json:"created_at,omitempty"`
}

// allBackupPrefixes is the authoritative catalogue of Badger key prefixes that
// Backup streams and Restore accepts (D-01). The INDEX of each prefix in this
// slice becomes the prefix_idx byte in the wire format — 0xFF is reserved as
// the EOF marker, so this slice MUST stay below 255 entries (currently 25).
//
// MUST be updated whenever a new prefix is introduced anywhere under
// pkg/metadata/store/badger/. Restore cross-checks the archive's declared
// key_prefix_list against this slice and rejects archives referencing
// prefixes unknown to the running binary (D-09 defensive check).
var allBackupPrefixes = []string{
	// encoding.go
	prefixFile,         // "f:"      File (JSON)
	prefixParent,       // "p:"      parent UUID index
	prefixChild,        // "c:"      directory child map
	prefixShare,        // "s:"      share root handle (JSON)
	prefixLinkCount,    // "l:"      link count (uint32 BE)
	prefixDeviceNumber, // "d:"      device number (JSON)
	prefixConfig,       // "cfg:"    server config singleton
	prefixCapabilities, // "cap:"    filesystem capabilities singleton

	// locks.go
	prefixLock,         // "lock:"     lock.LockStore primary records
	prefixLockByFile,   // "lkfile:"   index: file → locks
	prefixLockByOwner,  // "lkowner:"  index: owner → locks
	prefixLockByClient, // "lkclient:" index: client → locks
	prefixServerEpoch,  // "srvepoch"  singleton; no separator

	// clients.go
	prefixNSMClient,    // "nsm:client:"  NSM client registrations
	prefixNSMByMonName, // "nsm:monname:" index: monitor name → client

	// durable_handles.go
	prefixDHID,            // "dh:id:"    SMB3 durable handle primary
	prefixDHCreateGuid,    // "dh:cguid:" index: create-guid → id
	prefixDHAppInstanceId, // "dh:appid:" index: app-instance-id → id
	prefixDHFileID,        // "dh:fid:"   index: file-id → id
	prefixDHFileHandle,    // "dh:fh:"    index: file-handle → id
	prefixDHShare,         // "dh:share:" index: share → id

	// objects.go (FileBlockStore data)
	fileBlockPrefix,      // "fb:"       FileBlock primary
	fileBlockHashPrefix,  // "fb-hash:"  index: content hash → id
	fileBlockLocalPrefix, // "fb-local:" index: local cache key → id
	fileBlockFilePrefix,  // "fb-file:"  index: file → block(s)

	// transaction.go
	prefixFilesystemMeta, // "fsmeta:"   per-share filesystem meta (seeded lazily)
}

// Ensure BadgerMetadataStore implements metadata.Backupable.
var _ metadata.Backupable = (*BadgerMetadataStore)(nil)

func init() {
	// 0xFF is reserved as the EOF marker in the wire format, so the index
	// space for prefix_idx must stay strictly below it.
	if len(allBackupPrefixes) >= int(badgerEOFMarker) {
		panic(fmt.Sprintf("allBackupPrefixes has %d entries; must be < %d (0xFF reserved as EOF marker)",
			len(allBackupPrefixes), badgerEOFMarker))
	}
}

// ============================================================================
// Backup
// ============================================================================

// Backup streams a consistent snapshot of the store to w and returns the set
// of PayloadIDs referenced by regular files in that snapshot. The snapshot
// read-timestamp is taken when s.db.View opens; concurrent s.db.Update
// commits that land after that read-ts are invisible to both the PayloadID
// scan and the key/value emission loop (D-02 same-snapshot invariant).
//
// Backup deliberately avoids badger's DB-level streaming wrapper (the one
// that opens a distinct internal read-timestamp): such a wrapper cannot
// share its snapshot with a subsequent s.db.View used for the PayloadID
// scan. The resulting race window would violate D-02 and is prohibited by
// D-03, so we drive our own streaming loop inside a single s.db.View.
//
// On success the PayloadIDSet returned is safe for Phase 5's GC-hold: every
// regular file's non-empty PayloadID is included, and no extras. On error
// (writer failure, ctx cancellation, engine error) Backup returns an error
// wrapping metadata.ErrBackupAborted; w may be in a partial state and the
// caller MUST discard the partial archive.
func (s *BadgerMetadataStore) Backup(ctx context.Context, w io.Writer) (metadata.PayloadIDSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Header is static and does not need snapshot consistency — write it
	// first so a reader can fail fast on a truncated archive without having
	// consumed any frame bytes.
	if err := writeBadgerHeader(w); err != nil {
		return nil, fmt.Errorf("%w: write header: %v", metadata.ErrBackupAborted, err)
	}

	ids := metadata.NewPayloadIDSet()
	// CRC covers every byte between header and trailer. We feed the hasher
	// in parallel with the writer so the CRC stays cheap (no second pass).
	crc := crc32.NewIEEE()
	crcw := io.MultiWriter(w, crc)

	// D-02 + D-03: the prefixFile PayloadID scan and every prefix stream MUST
	// share a single db.View txn's read-timestamp. The closure is the trust
	// boundary — nothing inside it escapes to touch a different txn.
	err := s.db.View(func(txn *badgerdb.Txn) error {
		if err := scanPayloadIDsForBackup(ctx, txn, ids); err != nil {
			return err
		}
		for idx, prefix := range allBackupPrefixes {
			if err := streamPrefixForBackup(ctx, txn, uint8(idx), []byte(prefix), crcw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
		}
		return nil, fmt.Errorf("%w: %v", metadata.ErrBackupAborted, err)
	}

	// Trailer: EOF marker first so the decoder can disambiguate it from a
	// prefix_idx with a single byte-compare, then the CRC32 of all frame
	// bytes. Writing the trailer directly to w (not through crcw) keeps the
	// CRC authoritative — the marker and CRC itself are not part of the CRC
	// input.
	var trailer [5]byte
	trailer[0] = badgerEOFMarker
	binary.BigEndian.PutUint32(trailer[1:5], crc.Sum32())
	if _, err := w.Write(trailer[:]); err != nil {
		return nil, fmt.Errorf("%w: write trailer: %v", metadata.ErrBackupAborted, err)
	}
	return ids, nil
}

// writeBadgerHeader emits the length-prefixed JSON header.
func writeBadgerHeader(w io.Writer) error {
	hdr := badgerBackupHeader{
		FormatVersion: badgerFormatVersion,
		BadgerVersion: "v4",
		KeyPrefixList: append([]string(nil), allBackupPrefixes...),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(&hdr)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// scanPayloadIDsForBackup walks the prefixFile namespace under the caller's
// txn and accumulates every non-empty PayloadID into ids. Corrupt File
// entries are silently skipped to match the convention established by
// GetFileByPayloadID (files.go:73-75) — Backup is not the place to surface
// pre-existing data corruption, and aborting here would leave a single bad
// record blocking all DR recovery.
func scanPayloadIDsForBackup(ctx context.Context, txn *badgerdb.Txn, ids metadata.PayloadIDSet) error {
	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.Prefix = []byte(prefixFile)
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Rewind(); it.ValidForPrefix([]byte(prefixFile)); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := it.Item().Value(func(val []byte) error {
			f, decErr := decodeFile(val)
			if decErr != nil {
				return nil
			}
			if f.PayloadID != "" {
				ids.Add(string(f.PayloadID))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// streamPrefixForBackup emits one frame per key/value pair under prefix into
// w, tagged with prefixIdx. Iteration is scoped to the caller's txn, so all
// frames are at the same read-timestamp as the PayloadID scan above.
func streamPrefixForBackup(ctx context.Context, txn *badgerdb.Txn, prefixIdx uint8, prefix []byte, w io.Writer) error {
	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Rewind(); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := it.Item()
		key := item.KeyCopy(nil)
		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		if err := writeBackupFrame(w, prefixIdx, key, val); err != nil {
			return err
		}
	}
	return nil
}

// writeBackupFrame emits a single length-prefixed {prefix_idx, key, value}
// record. Buffers in a single Write call to minimise system-call overhead
// on large archives.
func writeBackupFrame(w io.Writer, prefixIdx uint8, k, v []byte) error {
	var hdr [9]byte
	hdr[0] = prefixIdx
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(k)))
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(v)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(k); err != nil {
		return err
	}
	if _, err := w.Write(v); err != nil {
		return err
	}
	return nil
}

// ============================================================================
// Restore
// ============================================================================

// Restore decodes an archive produced by Backup into this store. The store
// MUST be empty — any existing key with prefix "f:" causes Restore to abort
// with metadata.ErrRestoreDestinationNotEmpty before touching r (D-06).
//
// The caller is responsible for quiescing the store before invoking Restore.
// Phase 5's orchestrator constructs a fresh store instance in a temp path and
// calls Restore against it; Phase 2's driver does not attempt to drain an
// active store.
//
// Any decoding failure (truncation, bit-flip detected by CRC, oversized
// frame, out-of-range prefix_idx, unknown prefix declared in the header,
// unsupported format_version) is wrapped with metadata.ErrRestoreCorrupt so
// callers can match via errors.Is. The implementation buffers every decoded
// frame into a badger WriteBatch and only calls wb.Flush() after the CRC
// trailer validates; the deferred wb.Cancel() drops the batch on any
// pre-flush error, so the destination stays empty unless the entire archive
// verifies. A failure during wb.Flush() itself can leave partial state — that
// signals engine-level corruption and the destination must be discarded.
func (s *BadgerMetadataStore) Restore(ctx context.Context, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// D-06: reject any non-empty destination.
	nonEmpty, err := badgerDestinationHasFiles(s.db)
	if err != nil {
		return fmt.Errorf("check empty destination: %w", err)
	}
	if nonEmpty {
		return metadata.ErrRestoreDestinationNotEmpty
	}

	hdr, err := readBadgerHeader(r)
	if err != nil {
		return fmt.Errorf("%w: read header: %v", metadata.ErrRestoreCorrupt, err)
	}
	if hdr.FormatVersion == 0 || hdr.FormatVersion > badgerFormatVersion {
		return fmt.Errorf("%w: unsupported format_version=%d", metadata.ErrRestoreCorrupt, hdr.FormatVersion)
	}
	known := make(map[string]struct{}, len(allBackupPrefixes))
	for _, p := range allBackupPrefixes {
		known[p] = struct{}{}
	}
	for _, p := range hdr.KeyPrefixList {
		if _, ok := known[p]; !ok {
			return fmt.Errorf("%w: archive lists unknown key prefix %q", metadata.ErrRestoreCorrupt, p)
		}
	}

	wb := s.db.NewWriteBatch()
	defer wb.Cancel()

	// Feed a running CRC as we decode. The CRC is validated only after the
	// EOF marker has been consumed cleanly, so a truncated archive trips the
	// unexpected-EOF branch rather than a checksum mismatch.
	crc := crc32.NewIEEE()
	var one [1]byte
	lenBuf := make([]byte, 8)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, err := io.ReadFull(r, one[:]); err != nil {
			// Truncation before EOF marker is corruption by definition.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: unexpected eof before marker", metadata.ErrRestoreCorrupt)
			}
			return fmt.Errorf("%w: read prefix_idx: %v", metadata.ErrRestoreCorrupt, err)
		}

		// The EOF marker is 0xFF, which cannot be a legitimate prefix_idx
		// (len(allBackupPrefixes) < 255). Finding it here means the frame
		// stream is complete and the next 4 bytes must be the CRC trailer.
		if one[0] == badgerEOFMarker {
			var crcBuf [4]byte
			if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
				return fmt.Errorf("%w: read crc trailer: %v", metadata.ErrRestoreCorrupt, err)
			}
			declared := binary.BigEndian.Uint32(crcBuf[:])
			if declared != crc.Sum32() {
				return fmt.Errorf("%w: crc mismatch (declared=%08x computed=%08x)",
					metadata.ErrRestoreCorrupt, declared, crc.Sum32())
			}
			break
		}

		prefixIdx := one[0]
		if int(prefixIdx) >= len(hdr.KeyPrefixList) {
			return fmt.Errorf("%w: prefix_idx %d out of range (len=%d)",
				metadata.ErrRestoreCorrupt, prefixIdx, len(hdr.KeyPrefixList))
		}
		// prefix_idx is counted into the CRC (it was fed to the writer's CRC).
		crc.Write(one[:])

		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return fmt.Errorf("%w: read lengths: %v", metadata.ErrRestoreCorrupt, err)
		}
		crc.Write(lenBuf)
		keyLen := binary.BigEndian.Uint32(lenBuf[0:4])
		valLen := binary.BigEndian.Uint32(lenBuf[4:8])

		if keyLen == 0 || keyLen > badgerMaxKeyLen {
			return fmt.Errorf("%w: key_len %d out of bounds", metadata.ErrRestoreCorrupt, keyLen)
		}
		if valLen > badgerMaxValueLen {
			return fmt.Errorf("%w: value_len %d out of bounds", metadata.ErrRestoreCorrupt, valLen)
		}

		key := make([]byte, keyLen)
		if _, err := io.ReadFull(r, key); err != nil {
			return fmt.Errorf("%w: read key: %v", metadata.ErrRestoreCorrupt, err)
		}
		crc.Write(key)

		val := make([]byte, valLen)
		if valLen > 0 {
			if _, err := io.ReadFull(r, val); err != nil {
				return fmt.Errorf("%w: read value: %v", metadata.ErrRestoreCorrupt, err)
			}
			crc.Write(val)
		}

		expectedPrefix := hdr.KeyPrefixList[prefixIdx]
		if !bytes.HasPrefix(key, []byte(expectedPrefix)) {
			return fmt.Errorf("%w: key %q does not match declared prefix %q",
				metadata.ErrRestoreCorrupt, key, expectedPrefix)
		}

		if err := wb.Set(key, val); err != nil {
			return fmt.Errorf("writebatch set: %w", err)
		}
	}

	if err := wb.Flush(); err != nil {
		return fmt.Errorf("writebatch flush: %w", err)
	}

	// Phase 5 D-06: re-anchor the receiver's store_id after restore. The
	// cfg:store_id key is intentionally NOT in allBackupPrefixes, so the
	// source archive does not carry the source store's identity. However,
	// if a future format change ever includes it (or if a hand-crafted
	// archive smuggles it in), the re-anchor below is defense-in-depth:
	// the receiver's existing storeID wins unconditionally.
	//
	// This preserves the Phase 5 D-06 invariant: "an opened engine
	// instance's identity is fixed for its lifetime" — a fresh side-engine
	// opened by the restore orchestrator keeps its own ULID regardless of
	// the archive's contents.
	if err := s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte(storeIDKey), []byte(s.storeID))
	}); err != nil {
		return fmt.Errorf("re-anchor store_id: %w", err)
	}

	// Rebuild the in-memory used-bytes counter from the restored file set so
	// the store reports correct statistics without requiring a process
	// restart. This mirrors the initUsedBytesCounter invocation in the
	// constructor.
	if err := s.initUsedBytesCounter(); err != nil {
		return fmt.Errorf("recompute used-bytes counter: %w", err)
	}
	return nil
}

// readBadgerHeader parses the length-prefixed JSON header. Bounds the header
// size to prevent a malicious archive from forcing a multi-GB allocation.
func readBadgerHeader(r io.Reader) (*badgerBackupHeader, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	headerLen := binary.BigEndian.Uint32(lenBuf[:])
	if headerLen == 0 || headerLen > badgerMaxHeaderLen {
		return nil, fmt.Errorf("header_len %d out of bounds", headerLen)
	}
	body := make([]byte, headerLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var hdr badgerBackupHeader
	if err := json.Unmarshal(body, &hdr); err != nil {
		return nil, err
	}
	return &hdr, nil
}

// badgerDestinationHasFiles returns true iff any key with prefix "f:" exists
// in db — the D-06 empty-destination probe. Uses PrefetchValues=false so
// the probe is O(1) in value bytes.
func badgerDestinationHasFiles(db *badgerdb.DB) (bool, error) {
	var found bool
	err := db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixFile)
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		it.Rewind()
		if it.ValidForPrefix([]byte(prefixFile)) {
			found = true
		}
		return nil
	})
	return found, err
}
