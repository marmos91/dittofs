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
//	0    1     MagicByte    0xC1, torn-write scan anchor and layout tag
//	1    2     FileIDLen
//	3    8     FileOffset
//	11   8     Length
//	19   8     Version
//	27   1     Provenance   what Version dates; see coldProvenance
//	28   4     CRC32        over bytes [0,28) and the FileID bytes
//	32   var   FileID
//
// Entries written before provenance was recorded carry magic 0xC0 and lack the
// byte, putting CRC at [27,31) and the FileID at 31. They still load, as
// coldFromUnknown. Only the reader accepts that shape: every append writes 0xC1,
// so a log stops growing older entries as soon as this build touches it.
//
// A build that predates 0xC1 reads it as bad magic, which is a torn entry, which
// ends the load and drops every entry after it — the silent-zeros failure this
// log exists to prevent. That is why formatVersion is 2: an older release must
// refuse the directory outright rather than read it short.
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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

const (
	// coldMagicLegacy tags an entry written before provenance was recorded; it
	// is read, never written. coldMagic is the current layout.
	coldMagicLegacy      = 0xC0
	coldHeaderSizeLegacy = 31

	coldMagic      = 0xC1
	coldHeaderSize = 32
	coldLogName    = "cold.log"

	// coldSeededName is the marker recording that this journal's cold log has
	// been seeded from the caller's manifest. See ColdSeeded.
	coldSeededName = "cold-seeded"

	// coldCompactFloor keeps a compaction from rewriting a log that is merely
	// small: a rewrite only pays once the dead entries outweigh a few pages of
	// I/O. Both passes apply it through coldCompactWorthIt.
	coldCompactFloor = 1024

	// coldCompactWarnEvery is how many consecutive blocked passes it takes to
	// raise the report from Debug to Warn. A single block is a collision with an
	// appender and resolves itself; a run of them is compaction not happening,
	// which is invisible otherwise because the log just keeps growing.
	coldCompactWarnEvery = 20
)

// coldProvenance records which writer made an entry, because that is what says
// whether its version dates the *content* or merely the moment a scan noticed
// the range was remote-durable. Nothing else in the entry distinguishes them,
// and a reader that assumes one gets the other silently wrong.
type coldProvenance uint8

const (
	// coldFromUnknown is a legacy entry: written before provenance was recorded,
	// so which writer made it cannot be recovered. Readers must treat it as the
	// eviction case, which is what every reader did before this field existed.
	coldFromUnknown coldProvenance = 0

	// coldFromData means version was copied off the interval whose local bytes
	// this entry replaces, so it still dates the content: eviction and
	// invalidation. A version test answers "did these bytes exist at V".
	coldFromData coldProvenance = 1

	// coldFromScan means version was minted when a scan found the range already
	// remote-durable, so it dates the scan and says nothing about the content:
	// the manifest-seed writers. The version exists only so a racing Delete
	// sweeps below the entry. A version test answers nothing about the content.
	coldFromScan coldProvenance = 2
)

func (p coldProvenance) String() string {
	switch p {
	case coldFromData:
		return "data"
	case coldFromScan:
		return "scan"
	default:
		return "unknown"
	}
}

// coldEntry is one persisted cold interval.
type coldEntry struct {
	id         FileID
	fileOff    int64
	length     int64
	version    uint64
	provenance coldProvenance
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
	buf[27] = byte(e.provenance)
	copy(buf[coldHeaderSize:], e.id)
	binary.LittleEndian.PutUint32(buf[28:32], coldEntryCRC(buf[:28], buf[coldHeaderSize:]))
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
	if len(buf) < 1 {
		return coldEntry{}, 0, fmt.Errorf("%w: short cold entry header", errTornRecord)
	}

	// The magic byte doubles as the layout tag, so the header size and the
	// offsets of the trailing fields follow from it.
	var headerSize, crcOff int
	switch buf[0] {
	case coldMagic:
		headerSize, crcOff = coldHeaderSize, 28
	case coldMagicLegacy:
		headerSize, crcOff = coldHeaderSizeLegacy, 27
	default:
		return coldEntry{}, 0, fmt.Errorf("%w: bad cold entry magic 0x%02x", errTornRecord, buf[0])
	}
	if len(buf) < headerSize {
		return coldEntry{}, 0, fmt.Errorf("%w: short cold entry header", errTornRecord)
	}

	idLen := int(binary.LittleEndian.Uint16(buf[1:3]))
	total := headerSize + idLen
	if len(buf) < total {
		return coldEntry{}, 0, fmt.Errorf("%w: cold entry runs past end of log", errTornRecord)
	}
	id := buf[headerSize:total]
	want := binary.LittleEndian.Uint32(buf[crcOff : crcOff+4])
	if got := coldEntryCRC(buf[:crcOff], id); got != want {
		return coldEntry{}, 0, fmt.Errorf("%w: cold entry CRC mismatch", errTornRecord)
	}

	// A legacy entry has no provenance byte, so it stays coldFromUnknown.
	provenance := coldFromUnknown
	if buf[0] == coldMagic {
		provenance = coldProvenance(buf[27])
	}
	return coldEntry{
		id:         FileID(id),
		fileOff:    int64(binary.LittleEndian.Uint64(buf[3:11])),
		length:     int64(binary.LittleEndian.Uint64(buf[11:19])),
		version:    binary.LittleEndian.Uint64(buf[19:27]),
		provenance: provenance,
	}, total, nil
}

// appendCold durably records entries so a restart still knows the ranges are
// remote-resident rather than holes.
//
// It fsyncs before returning because of the eviction caller: that caller is about
// to unlink the only local copy of these bytes, and a lost entry means silent
// zeros rather than a slow read. The seed caller is idempotent — an interrupted
// seed leaves no ColdSeeded marker and repeats — so its appends may be batched.
// Batching both loses the eviction caller's only durable record of a copy it is
// about to unlink.
//
// Entries are already a batch: a caller that may batch does so by accumulating
// entries and calling this once, never by relaxing what this does with them.
func (s *Store) appendCold(entries []coldEntry) error {
	if len(entries) == 0 {
		return nil
	}
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	// A broken log has no appendable tail: replay ends at the tear, so putting
	// entries behind it would lose them for good. The caller (demote) surfaces
	// this as a refused demotion, which every caller treats as fail-closed.
	if s.coldBroken {
		return fmt.Errorf("journal: cold log unusable after a failed tail rollback")
	}
	if s.coldFD == nil {
		_, statErr := os.Stat(s.coldPath())
		created := errors.Is(statErr, os.ErrNotExist)
		fd, err := os.OpenFile(s.coldPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("journal: open cold log: %w", err)
		}
		// Syncing the file does not make its directory entry durable. Without
		// this, a crash right after the first eviction can come back to no
		// cold.log at all — the exact loss this file exists to prevent.
		if created {
			if derr := fsyncDir(s.dir); derr != nil {
				_ = fd.Close()
				return fmt.Errorf("journal: fsync dir for cold log: %w", derr)
			}
		}
		s.coldFD = fd
	}
	var (
		buf     []byte
		written int
	)
	for _, e := range entries {
		if e.length <= 0 {
			continue
		}
		if len(e.id) > maxFileIDLen {
			// FileIDLen is uint16 on disk; a longer ID would be silently
			// truncated and make every following entry unreplayable.
			return fmt.Errorf("journal: cold log FileID length %d exceeds max %d", len(e.id), maxFileIDLen)
		}
		buf = append(buf, encodeColdEntry(e)...)
		written++
	}
	if len(buf) == 0 {
		return nil
	}
	// A write that fails part-way (ENOSPC is the likely one: eviction runs because
	// the volume this log shares with the segments is filling up) leaves a torn
	// entry at the tail. Replay stops at the first tear, so any entry a later
	// append puts behind it is lost for good — silent zeros for a range the caller
	// went on to evict. Roll the tail back to where the batch started so the log
	// keeps its "only the tail can tear" shape and the caller's retry appends onto
	// intact bytes.
	st, err := s.coldFD.Stat()
	if err != nil {
		return fmt.Errorf("journal: stat cold log: %w", err)
	}
	tail := st.Size()
	if _, err := s.coldFD.Write(buf); err != nil {
		// The rollback itself can fail (ENOSPC on the metadata write): if it
		// does, the log keeps a torn tail and every entry a later append puts
		// behind it would be lost for good. Mark the log broken so no later
		// append rides after the tear; a restart replays exactly what fsynced.
		terr := s.coldFD.Truncate(tail)
		if terr != nil {
			s.coldBroken = true
			return errors.Join(fmt.Errorf("journal: append cold log: %w", err),
				fmt.Errorf("journal: cold log marked unusable: rollback failed: %w", terr))
		}
		return errors.Join(fmt.Errorf("journal: append cold log: %w", err), terr)
	}
	if err := s.coldFD.Sync(); err != nil {
		terr := s.coldFD.Truncate(tail)
		if terr != nil {
			s.coldBroken = true
			return errors.Join(fmt.Errorf("journal: fsync cold log: %w", err),
				fmt.Errorf("journal: cold log marked unusable: rollback failed: %w", terr))
		}
		return errors.Join(fmt.Errorf("journal: fsync cold log: %w", err), terr)
	}
	// Counted only once the bytes are fsynced, so the count describes the log a
	// restart would read rather than one a rolled-back write left behind.
	s.coldEntries += written
	return nil
}

// loadCold reads every intact entry from dir's cold log. A missing log is not an
// error (no eviction or seed has happened yet). It stops at the first torn entry
// and returns what precedes it, along with the byte offset just past the last
// intact entry: anything between that offset and the end of the file is garbage
// the caller is expected to drop with truncateColdTail before appending.
func loadCold(dir string, log *slog.Logger) ([]coldEntry, int64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, coldLogName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("journal: read cold log: %w", err)
	}
	var out []coldEntry
	off := 0
	for off < len(raw) {
		e, n, derr := decodeColdEntry(raw[off:])
		if derr != nil {
			log.Warn("journal: cold log torn, keeping the intact entries",
				"offset", off, "intact_entries", len(out), "err", derr)
			break
		}
		out = append(out, e)
		off += n
	}
	return out, int64(off), nil
}

// truncateColdTail drops whatever follows the last intact entry and makes the
// truncation durable. The log is only ever opened for append, so a torn tail
// left in place sits in front of every later entry: the next load stops at the
// same offset and discards everything appended in between, turning one torn
// write into the permanent loss of every cold interval recorded after it.
func truncateColdTail(dir string, validUpTo int64, log *slog.Logger) error {
	fd, err := os.OpenFile(filepath.Join(dir, coldLogName), os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("journal: open cold log for tail repair: %w", err)
	}
	defer func() { _ = fd.Close() }()
	st, err := fd.Stat()
	if err != nil {
		return fmt.Errorf("journal: stat cold log: %w", err)
	}
	if st.Size() <= validUpTo {
		return nil
	}
	log.Warn("journal: dropping torn cold log tail",
		"valid_up_to", validUpTo, "dropped_bytes", st.Size()-validUpTo)
	if err := fd.Truncate(validUpTo); err != nil {
		return fmt.Errorf("journal: truncate torn cold log tail: %w", err)
	}
	if err := fd.Sync(); err != nil {
		return fmt.Errorf("journal: fsync truncated cold log: %w", err)
	}
	return nil
}

// rewriteCold replaces the log with exactly entries, dropping the ones a
// compaction found superseded, deleted or truncated away. Written to a temp file
// and renamed so a crash mid-rewrite leaves the previous log intact.
func (s *Store) rewriteCold(entries []coldEntry) error {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	return s.rewriteColdLocked(entries)
}

// rewriteColdLocked is rewriteCold with coldMu already held, for a caller that
// has to decide whether to rewrite under the same hold that protects the
// decision (maybeCompactColdLog).
//
// ponytail: the temp write and both fsyncs run under coldMu, so a seed or an
// invalidate waits out the whole rewrite while holding its shard lock — the
// stall Invalidate's own marker calls out for one append, multiplied by the
// surviving log. Only the rename has to be atomic against an append: build the
// temp file before taking coldMu, and re-verify there, if a shard is measured
// stalling on a compaction.
func (s *Store) rewriteColdLocked(entries []coldEntry) error {
	if s.coldFD != nil {
		_ = s.coldFD.Close()
		s.coldFD = nil
	}
	path := s.coldPath()
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				s.coldEntries = 0
				return nil
			}
			return fmt.Errorf("journal: remove cold log: %w", err)
		}
		if err := fsyncDir(s.dir); err != nil {
			return fmt.Errorf("journal: fsync dir after cold log removal: %w", err)
		}
		// Counted only once the file is actually gone, exactly as the rewrite
		// below counts only a log it finished renaming: a refused unlink leaves N
		// entries on disk, and a counter reading zero would let the next
		// compaction rewrite from a snapshot it never verified against them.
		s.coldEntries = 0
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
	// The rename itself needs a directory flush to be durable; without it a
	// crash can leave the pre-compaction log — or neither file.
	if err := fsyncDir(s.dir); err != nil {
		return fmt.Errorf("journal: fsync dir after cold log rewrite: %w", err)
	}
	s.coldEntries = len(entries)
	return nil
}

// appendColdPublish records entries in the log and then publishes them into the
// index as one unit, marked in flight for as long as the two disagree.
//
// The halves are deliberately separated in two callers: eviction scans under the
// shard lock, drops it, appends, and re-takes the lock to flip the intervals
// cold, and a batch seed appends once for many files and then takes each shard
// lock in turn to insert. In that gap the log holds entries no index shows as
// cold, so a compaction that snapshotted the index there would write a log
// missing exactly them — and the appender goes on to unlink their bytes. They
// would come back from a restart as holes that read as zeros with no fetch.
//
// Refusing while in flight is the whole guard: the count is raised before the
// append, so an entry the log holds is either already published or in flight and
// keeping the compactor off the log. A caller that returned without releasing
// would stall compaction rather than lose an entry.
func (s *Store) appendColdPublish(entries []coldEntry, publish func()) error {
	s.coldMu.Lock()
	s.coldInFlight++
	hook := s.betweenColdAppendAndPublish
	s.coldMu.Unlock()
	defer func() {
		s.coldMu.Lock()
		s.coldInFlight--
		s.coldMu.Unlock()
	}()
	if err := s.appendCold(entries); err != nil {
		return err
	}
	if hook != nil {
		hook()
	}
	publish()
	return nil
}

// coldCompactWorthIt reports whether a log holding logged entries is worth
// rewriting down to a live set of live entries: the dead weight has to outweigh
// what would be rewritten, and pay for at least a few pages of I/O. Recovery and
// the background pass share the predicate so a log that one of them would leave
// alone is not rewritten by the other on the next tick.
func coldCompactWorthIt(logged, live int) bool {
	return logged > 2*live+coldCompactFloor
}

// maybeCompactColdLog rewrites the cold log down to the entries still live. It
// runs off the GC ticker because eviction and seeding only ever append: a range
// evicted, hydrated back and evicted again leaves its first entry behind as dead
// weight, so without this the log grows for as long as the process stays up and
// a restart pays for all of it in loadCold.
//
// Cold-log callers take their shard lock before coldMu (SeedCold and Invalidate
// hold it across the append), so this must never hold coldMu across a shard
// lock. It snapshots the live set shard by shard instead and then verifies,
// under coldMu, that the log it is about to replace is still the one it
// snapshotted: nothing appended, and nothing appended-but-not-yet-indexed
// (appendColdPublish). If either moved it abandons the pass, because rewriting
// without an entry some caller is relying on is silent zeros for a range that
// caller has already stopped keeping locally.
//
// A shard whose records are not all fsynced yet is not a reason to abandon the
// pass, only a reason not to drop anything of that shard's: its entries are
// carried over from the log untouched. Keeping an entry that is already dead
// costs a replay that resolves it away — recovery inserts by Version, so a later
// record shadows it and a tombstone or truncate clips it — which is the same
// reason a crash before the rename is harmless. So each pass shrinks what it
// could verify and leaves the rest for the next one.
//
// ponytail: the carried set is read back with loadCold rather than kept in
// memory, so a pass over a log with any unverified shard costs one read of it;
// that read is bounded by what this pass keeps the log to. Hold the log's entry
// set in memory if that read ever shows up next to the fsyncs it sits between.
func (s *Store) maybeCompactColdLog() {
	if s.closed.Load() {
		return
	}
	s.coldMu.Lock()
	logged, broken, inFlight := s.coldEntries, s.coldBroken, s.coldInFlight
	verify := s.beforeColdCompactVerify
	s.coldMu.Unlock()
	// The size gate keeps an idle or small log from costing a shard walk every
	// tick. It is not a refusal: there is nothing to do.
	if !coldCompactWorthIt(logged, 0) {
		s.clearColdCompactBlocked()
		return
	}
	// A broken log has no appendable tail and must not be rewritten from a
	// snapshot either: replay ends at its tear, so the live set is not what the
	// log describes.
	if broken {
		s.blockColdCompact("the cold log is unusable after a failed tail rollback", logged)
		return
	}
	if inFlight > 0 {
		s.blockColdCompact("an append is between the log and the index", logged)
		return
	}
	// decision: an entry is dropped because a record, tombstone or truncate
	// marker superseded it, and dropping it is only safe once that marker is
	// itself durable — a lost record leaves the range a hole, which is the
	// silent-zeros failure the log exists to prevent. Delete and Truncate fsync
	// their markers before touching the index, but a plain record is durable only
	// after a shard commit, so ask for one here rather than waiting for the
	// dirty-age loop to have happened to run. It decides how much of the store a
	// pass can shrink, not whether the pass runs: a shard still dirty afterwards
	// keeps its entries rather than blocking the others. A negative DirtyExpiry
	// is an operator saying no loop may fsync on its own schedule, and that
	// answer is taken here too — such a store shrinks only the shards a client
	// commit happens to have cleaned.
	if s.cfg.DirtyExpiry > 0 {
		if err := s.commitDirtyShards(); err != nil {
			s.log.Warn("journal: cold log compaction skipped, shard commit failed", "error", err)
			return
		}
	}
	live, unverified := s.liveColdSnapshot()
	keep, loadedCount := live, -1
	if slices.Contains(unverified, true) {
		loaded, _, err := loadCold(s.dir, s.log)
		if err != nil {
			s.blockColdCompact("the cold log could not be read back", logged)
			return
		}
		loadedCount = len(loaded)
		for _, e := range loaded {
			if unverified[s.shardIndex(e.id)] {
				keep = append(keep, e)
			}
		}
	}
	if !coldCompactWorthIt(logged, len(keep)) {
		s.clearColdCompactBlocked()
		return
	}
	if verify != nil {
		verify()
	}
	s.coldMu.Lock()
	// An append only ever raises the count, so an unchanged count means the
	// snapshot still describes the whole log; an append in flight means the index
	// it was taken from does not describe entries the log already holds. Either
	// way those entries are the ones a rewrite would drop, so leave the log to
	// the next pass. Nothing appended over the window also makes the log the
	// exact one loadCold read, so a count that disagrees with it is the counter
	// having drifted from the file, and the decision above rests on the counter.
	moved := s.coldEntries != logged || s.coldInFlight > 0
	drifted := loadedCount >= 0 && loadedCount != logged && !moved
	if moved || drifted {
		s.coldRefusals++
		n := s.coldRefusals
		s.coldMu.Unlock()
		if drifted {
			s.log.Warn("journal: cold log entry count disagrees with the log on disk, skipping compaction",
				"counted", logged, "on_disk", loadedCount)
			return
		}
		s.logColdCompactBlocked(n, "the log moved under the snapshot", logged)
		return
	}
	s.coldRefusals = 0
	err := s.rewriteColdLocked(keep)
	s.coldMu.Unlock()
	if err != nil {
		// Non-fatal for the same reason recovery's compaction is: a
		// stale-but-valid log costs redundant replay, not correctness.
		s.log.Warn("journal: cold log compaction failed, keeping the existing log", "error", err)
	}
}

// blockColdCompact records a pass that could not get to a verdict and says so:
// at Debug for one, and at Warn once a run of them means compaction has stopped
// happening at all. Every such reason fails closed — the log keeps growing while
// nothing shrinks it — so the run length is the only thing that distinguishes a
// collision with an appender from a store where an append never released.
// coldMu must not be held.
func (s *Store) blockColdCompact(reason string, logged int) {
	s.coldMu.Lock()
	s.coldRefusals++
	n := s.coldRefusals
	s.coldMu.Unlock()
	s.logColdCompactBlocked(n, reason, logged)
}

// logColdCompactBlocked is blockColdCompact's reporting half, for the caller
// that already bumped the run under coldMu.
func (s *Store) logColdCompactBlocked(n int, reason string, logged int) {
	if n%coldCompactWarnEvery == 0 {
		s.log.Warn("journal: cold log compaction has been blocked for consecutive passes",
			"reason", reason, "passes", n, "entries", logged)
		return
	}
	s.log.Debug("journal: cold log compaction deferred", "reason", reason, "passes", n, "entries", logged)
}

// clearColdCompactBlocked ends a run of blocked passes: the pass reached a
// verdict, whether or not it had anything to do. coldMu must not be held.
func (s *Store) clearColdCompactBlocked() {
	s.coldMu.Lock()
	s.coldRefusals = 0
	s.coldMu.Unlock()
}

// liveColdSnapshot gathers the live cold entries a shard at a time, each under
// that shard's own lock and never with coldMu held: a caller that holds a shard
// lock across its append takes coldMu second, so taking them the other way round
// here would invert that.
//
// A shard is skipped, and reported in the returned mask, when it still holds a
// record no fsync has covered. The entries a snapshot leaves out are the ones a
// rewrite drops, and an entry superseded by a record that can still be lost must
// be kept: losing the record would turn the range into a hole that reads as
// zeros with no fetch. The caller carries a skipped shard's entries over from
// the log instead, so one dirty shard costs that shard's dead weight rather than
// the whole pass.
func (s *Store) liveColdSnapshot() ([]coldEntry, []bool) {
	var live []coldEntry
	unverified := make([]bool, len(s.shards))
	for i, sh := range s.shards {
		sh.mu.Lock()
		if sh.lastVersion <= sh.syncedVersion.Load() {
			live = append(live, liveColdEntries([]map[FileID]*fileIndex{sh.index})...)
		} else {
			unverified[i] = true
		}
		sh.mu.Unlock()
	}
	return live, unverified
}

// liveColdEntries collects the cold intervals an index walk ended up with, which
// is the set a compaction should keep. It locks nothing; its callers say why.
func liveColdEntries(indexByShard []map[FileID]*fileIndex) []coldEntry {
	var out []coldEntry
	for _, idxMap := range indexByShard {
		for id, fi := range idxMap {
			for _, iv := range fi.ivs {
				if !iv.cold {
					continue
				}
				out = append(out, coldEntry{
					id: id, fileOff: iv.fileOff, length: iv.length,
					version: iv.version, provenance: iv.provenance,
				})
			}
		}
	}
	return out
}

// ColdSeeded reports whether a caller has already seeded this journal's cold
// log from its manifest and said so with MarkColdSeeded.
//
// A journal that opened empty over data living only on a remote store — an
// upgrade that archived the previous local layout aside, a restore that wiped
// the local tier — holds no interval for those ranges, so a read zero-fills
// instead of fetching. The repair is a manifest scan, which is O(files) and
// worth doing once rather than on every open. A store whose marker cannot be
// read is reported unseeded: repeating the scan costs time, skipping it serves
// zeros.
func (s *Store) ColdSeeded() bool {
	_, err := os.Stat(filepath.Join(s.dir, coldSeededName))
	return err == nil
}

// MarkColdSeeded records that the cold log has been seeded from a manifest.
// Call it only once the seed's entries are durable: an open interrupted before
// this point leaves no marker and seeds again, whereas a marker written early
// would let an incomplete seed be mistaken for a finished one.
func (s *Store) MarkColdSeeded() error {
	fd, err := os.OpenFile(filepath.Join(s.dir, coldSeededName), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("journal: create cold seed marker: %w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("journal: close cold seed marker: %w", err)
	}
	// The marker is an empty file, so only the directory entry has to reach disk.
	return fsyncDir(s.dir)
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
