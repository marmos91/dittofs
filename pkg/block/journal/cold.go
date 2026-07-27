package journal

// cold.go makes cold intervals survive a restart.
//
// A cold interval is a range that is durable on the remote store but holds no
// local bytes: eviction dropped the segment that held them, or a range was
// seeded cold because the local tier never had them (an upgrade that archived a
// pre-journal layout aside, a snapshot restore that wiped the tier). Unlike a
// warm interval it owns no record, and the interval index is rebuilt purely from
// segment records — so without a durable side-log every cold interval comes back
// from a restart as a POSIX hole, and a read zero-fills instead of fetching from
// the remote. That is a silent-wrong-bytes failure, not a slow read, so the log
// is fsynced before the eviction that needs it unlinks its segment.
//
// The log cannot live in the segment stream itself: a segment holding nothing
// but cold markers has no live warm interval, so eviction and GC would reclaim
// it and take the markers with it.
//
// Layout: <dir>/cold.log, an append-only entry stream, little-endian
//
//	off  size  field
//	0    1     MagicByte    0xC0, torn-write scan anchor
//	1    2     FileIDLen
//	3    8     FileOffset
//	11   8     Length
//	19   8     Version
//	27   4     CRC32        over bytes [0,27) and the FileID bytes
//	31   var   FileID
//
// Entries are replayed at recovery like any other record — inserted by Version,
// so a later warm write shadows a cold entry, and the tombstone and truncate
// markers clip them exactly as they clip warm intervals. A torn tail ends the
// load: only the tail can tear, and every entry before it is intact.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

const (
	coldMagic      = 0xC0
	coldHeaderSize = 31
	coldLogName    = "cold.log"

	// coldCompactFloor keeps recovery from rewriting a log that is merely small:
	// compaction only pays once the dead entries outweigh a few pages of I/O.
	coldCompactFloor = 1024
)

// coldEntry is one persisted cold interval.
type coldEntry struct {
	id      FileID
	fileOff int64
	length  int64
	version uint64
}

func (s *Store) coldPath() string { return filepath.Join(s.dir, coldLogName) }

// encodeColdEntry frames one entry.
func encodeColdEntry(e coldEntry) []byte {
	buf := make([]byte, coldHeaderSize+len(e.id))
	buf[0] = coldMagic
	binary.LittleEndian.PutUint16(buf[1:3], uint16(len(e.id)))
	binary.LittleEndian.PutUint64(buf[3:11], uint64(e.fileOff))
	binary.LittleEndian.PutUint64(buf[11:19], uint64(e.length))
	binary.LittleEndian.PutUint64(buf[19:27], e.version)
	copy(buf[coldHeaderSize:], e.id)
	binary.LittleEndian.PutUint32(buf[27:31], coldEntryCRC(buf[:27], buf[coldHeaderSize:]))
	return buf
}

// coldEntryCRC checksums an entry's header (excluding the CRC field itself) and
// its FileID bytes as one stream.
func coldEntryCRC(head, id []byte) uint32 {
	return crc32.Update(crc32.Checksum(head, crcTable), crcTable, id)
}

// decodeColdEntry parses one entry from the head of buf, returning it and the
// total bytes consumed. A malformed entry (bad magic, short read, CRC mismatch)
// returns errTornRecord, which ends the load.
func decodeColdEntry(buf []byte) (coldEntry, int, error) {
	if len(buf) < coldHeaderSize {
		return coldEntry{}, 0, fmt.Errorf("%w: short cold entry header", errTornRecord)
	}
	if buf[0] != coldMagic {
		return coldEntry{}, 0, fmt.Errorf("%w: bad cold entry magic 0x%02x", errTornRecord, buf[0])
	}
	idLen := int(binary.LittleEndian.Uint16(buf[1:3]))
	total := coldHeaderSize + idLen
	if len(buf) < total {
		return coldEntry{}, 0, fmt.Errorf("%w: cold entry runs past end of log", errTornRecord)
	}
	id := buf[coldHeaderSize:total]
	want := binary.LittleEndian.Uint32(buf[27:31])
	if got := coldEntryCRC(buf[:27], id); got != want {
		return coldEntry{}, 0, fmt.Errorf("%w: cold entry CRC mismatch", errTornRecord)
	}
	return coldEntry{
		id:      FileID(id),
		fileOff: int64(binary.LittleEndian.Uint64(buf[3:11])),
		length:  int64(binary.LittleEndian.Uint64(buf[11:19])),
		version: binary.LittleEndian.Uint64(buf[19:27]),
	}, total, nil
}

// appendCold durably records entries so a restart still knows the ranges are
// remote-resident rather than holes. It fsyncs before returning: the caller
// (eviction) is about to unlink the only local copy of those bytes, and a lost
// entry means silent zeros, not a slow read.
func (s *Store) appendCold(entries []coldEntry) error {
	if len(entries) == 0 {
		return nil
	}
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	if s.coldFD == nil {
		fd, err := os.OpenFile(s.coldPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("journal: open cold log: %w", err)
		}
		s.coldFD = fd
	}
	var buf []byte
	for _, e := range entries {
		if e.length <= 0 {
			continue
		}
		buf = append(buf, encodeColdEntry(e)...)
	}
	if len(buf) == 0 {
		return nil
	}
	if _, err := s.coldFD.Write(buf); err != nil {
		return fmt.Errorf("journal: append cold log: %w", err)
	}
	if err := s.coldFD.Sync(); err != nil {
		return fmt.Errorf("journal: fsync cold log: %w", err)
	}
	return nil
}

// loadCold reads every intact entry from dir's cold log. A missing log is not an
// error (no eviction or seed has happened yet). It stops at the first torn entry
// and returns what precedes it.
func loadCold(dir string) ([]coldEntry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, coldLogName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("journal: read cold log: %w", err)
	}
	var out []coldEntry
	for off := 0; off < len(raw); {
		e, n, derr := decodeColdEntry(raw[off:])
		if derr != nil {
			logf("journal: WARN cold log torn at offset %d, keeping %d intact entry(ies): %v", off, len(out), derr)
			break
		}
		out = append(out, e)
		off += n
	}
	return out, nil
}

// rewriteCold replaces the log with exactly entries, dropping the ones recovery
// found superseded, deleted or truncated away. Written to a temp file and
// renamed so a crash mid-rewrite leaves the previous log intact.
func (s *Store) rewriteCold(entries []coldEntry) error {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	if s.coldFD != nil {
		_ = s.coldFD.Close()
		s.coldFD = nil
	}
	path := s.coldPath()
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("journal: remove cold log: %w", err)
		}
		return nil
	}
	tmp := path + ".tmp"
	fd, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("journal: create cold log temp: %w", err)
	}
	var buf []byte
	for _, e := range entries {
		buf = append(buf, encodeColdEntry(e)...)
	}
	if _, err := fd.Write(buf); err != nil {
		_ = fd.Close()
		return fmt.Errorf("journal: write cold log temp: %w", err)
	}
	if err := fd.Sync(); err != nil {
		_ = fd.Close()
		return fmt.Errorf("journal: fsync cold log temp: %w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("journal: close cold log temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("journal: rename cold log: %w", err)
	}
	return nil
}

// liveColdEntries collects the cold intervals a recovery ended up with, which is
// the set a compaction should keep. Called during recovery, before the shards are
// published, so it needs no locking.
func liveColdEntries(indexByShard []map[FileID]*fileIndex) []coldEntry {
	var out []coldEntry
	for _, idxMap := range indexByShard {
		for id, fi := range idxMap {
			for _, iv := range fi.ivs {
				if !iv.cold {
					continue
				}
				out = append(out, coldEntry{id: id, fileOff: iv.fileOff, length: iv.length, version: iv.version})
			}
		}
	}
	return out
}

// closeCold releases the append handle. Idempotent.
func (s *Store) closeCold() error {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	if s.coldFD == nil {
		return nil
	}
	err := s.coldFD.Close()
	s.coldFD = nil
	return err
}
