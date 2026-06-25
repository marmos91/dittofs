package fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// Append-log on-disk format constants. See in
//
// Log layout
//
//	[0..64) fixed header (see logHeader + marshalHeader)
//	[64..) append-only stream of records (see writeRecord)
//
// Record framing
//
//	uint32 LE payload_len
//	uint64 LE file_offset (byte offset inside the logical payload)
//	uint32 LE crc32c (Castagnoli, over file_offset_8B_LE || payload)
//	[]byte payload (payload_len bytes)
//
// Recovery truncates at the first bad CRC or short record.
const (
	logHeaderSize       = 64
	recordFrameOverhead = 16 // payload_len(4) + file_offset(8) + crc(4)
	logVersion          = uint32(1)
	// maxRecordPayload (FIX-4) clamps the per-record payload allocation.
	// A torn-tail or hostile log frame can present an arbitrary 32-bit
	// payload_len; without a cap, recovery / rollup replay would attempt
	// a 4 GiB-1 allocation per bad frame → OOM / DoS. Set just above the
	// chunker's hard maximum (16 MiB) plus slack so legitimate records
	// always pass; anything larger is treated as corruption and the
	// caller truncates the log at the previous valid position.
	maxRecordPayload = 17 * 1024 * 1024
)

// logMagic is the fixed 4-byte prefix of every DittoFS append log: 'DFLG'.
var logMagic = [4]byte{'D', 'F', 'L', 'G'}

// crcTable is the Castagnoli CRC32 table used for record and header CRCs
// . Hardware-accelerated on amd64 (SSE4.2) and arm64 (ARMv8 CRC32
// extension).
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// logHeader is the 64-byte on-disk layout documented in.
//
// The fixed 32-byte reserved tail (bytes [32..64)) and the 4-byte header_crc
// (bytes [28..32)) are not represented as struct fields — they are handled
// directly in marshalHeader / unmarshalHeader.
type logHeader struct {
	Magic        [4]byte
	Version      uint32
	RollupOffset uint64
	Flags        uint32
	CreatedAt    int64
}

// marshalHeader encodes h into a fixed 64-byte buffer with the header CRC
// filled in over bytes [0..28) and bytes [32..64) zeroed (reserved).
func marshalHeader(h logHeader) [logHeaderSize]byte {
	var buf [logHeaderSize]byte
	copy(buf[0:4], h.Magic[:])
	binary.LittleEndian.PutUint32(buf[4:8], h.Version)
	binary.LittleEndian.PutUint64(buf[8:16], h.RollupOffset)
	binary.LittleEndian.PutUint32(buf[16:20], h.Flags)
	binary.LittleEndian.PutUint64(buf[20:28], uint64(h.CreatedAt))
	crc := crc32.Checksum(buf[0:28], crcTable)
	binary.LittleEndian.PutUint32(buf[28:32], crc)
	// buf[32:64] stays zeroed (reserved).
	return buf
}

// unmarshalHeader decodes a 64-byte log header from buf.
//
// Returns ErrLogBadMagic, ErrLogBadVersion, or ErrLogBadHeaderCRC on
// mismatch. Returns a plain fmt.Errorf if buf is too short to hold a full
// header.
func unmarshalHeader(buf []byte) (logHeader, error) {
	var h logHeader
	if len(buf) < logHeaderSize {
		return h, fmt.Errorf("append log: header short: %d < %d", len(buf), logHeaderSize)
	}
	copy(h.Magic[:], buf[0:4])
	if h.Magic != logMagic {
		return h, ErrLogBadMagic
	}
	h.Version = binary.LittleEndian.Uint32(buf[4:8])
	if h.Version != logVersion {
		return h, ErrLogBadVersion
	}
	wantCRC := binary.LittleEndian.Uint32(buf[28:32])
	gotCRC := crc32.Checksum(buf[0:28], crcTable)
	if wantCRC != gotCRC {
		return h, ErrLogBadHeaderCRC
	}
	h.RollupOffset = binary.LittleEndian.Uint64(buf[8:16])
	h.Flags = binary.LittleEndian.Uint32(buf[16:20])
	h.CreatedAt = int64(binary.LittleEndian.Uint64(buf[20:28]))
	return h, nil
}

// writeRecord writes one framed record to w. The on-disk layout is
// payload_len(uint32 LE) || file_offset(uint64 LE) || crc(uint32 LE) || payload.
// The CRC is computed over (file_offset_8B_LE || payload).
//
// Returns the number of bytes written (including both the 16-byte frame
// header and the payload).
//
// TRANSITIONAL-NEXT-MILESTONE: tmpfs spill (see #519 "Deferred to v0.17+").
// When tmpfs spill lands, the in-RAM appendlog buffer will spill to a
// memory-backed temp tier (typically /dev/shm) before disk, eliminating
// the SSD wear cost for log churn under burst write workloads. The
// current implementation spills directly to the on-disk log fd; tmpfs
// spill inserts an intermediate tier — writeRecord's signature stays
// (w io.Writer accepts either fd), only the caller's choice of `w`
// changes.
func writeRecord(w io.Writer, fileOffset uint64, payload []byte) (int, error) {
	if len(payload) > int(^uint32(0)) {
		return 0, fmt.Errorf("append log: payload too large (%d)", len(payload))
	}
	var frame [recordFrameOverhead]byte
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint64(frame[4:12], fileOffset)

	// CRC over (file_offset_8B_LE || payload).
	var offBuf [8]byte
	binary.LittleEndian.PutUint64(offBuf[:], fileOffset)
	crc := crc32.Update(0, crcTable, offBuf[:])
	crc = crc32.Update(crc, crcTable, payload)
	binary.LittleEndian.PutUint32(frame[12:16], crc)

	n1, err := w.Write(frame[:])
	if err != nil {
		return n1, fmt.Errorf("append log: write frame: %w", err)
	}
	n2, err := w.Write(payload)
	if err != nil {
		return n1 + n2, fmt.Errorf("append log: write payload: %w", err)
	}
	return n1 + n2, nil
}

// readRecord reads one framed record from r.
//
// Return semantics
//   - (off, payload, true, nil) on success.
//   - (0, nil, false, nil) on clean EOF, short read, length-overflow, or CRC
//     mismatch — caller truncates the log at the previous valid position
//   - (0, nil, false, err) only on hard I/O errors (not EOF / CRC).
func readRecord(r io.Reader) (uint64, []byte, bool, error) {
	var frame [recordFrameOverhead]byte
	if _, err := io.ReadFull(r, frame[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("append log: read frame: %w", err)
	}
	payloadLen := binary.LittleEndian.Uint32(frame[0:4])
	fileOffset := binary.LittleEndian.Uint64(frame[4:12])
	wantCRC := binary.LittleEndian.Uint32(frame[12:16])
	// FIX-4: clamp via the file-scope maxRecordPayload cap (defined
	// alongside the framing constants).
	if payloadLen > maxRecordPayload {
		return 0, nil, false, nil
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("append log: read payload: %w", err)
	}
	var offBuf [8]byte
	binary.LittleEndian.PutUint64(offBuf[:], fileOffset)
	gotCRC := crc32.Update(0, crcTable, offBuf[:])
	gotCRC = crc32.Update(gotCRC, crcTable, payload)
	if gotCRC != wantCRC {
		return 0, nil, false, nil
	}
	return fileOffset, payload, true, nil
}

// readRecordAt reads the framed record whose first byte sits at logPos
// in rf. payloadLen is the caller-known payload size (the logIndex
// entry's payloadLen field), so we issue a single pread of the full
// record (frame header + payload) instead of two sequential reads.
//
// Returns the decoded file_offset and the payload bytes. Errors
//   - payloadLen exceeds the maxRecordPayload DoS cap (also enforced
//     by readRecord; mirrored here so a pathological logIndex entry
//     cannot drive a multi-GB allocation if memory corruption ever
//     forged one)
//   - the pread returned an I/O error / short read
//   - the frame's declared payload_len disagrees with the index
//   - CRC over (file_offset || payload) mismatches the stored CRC
//
// All cases indicate either log-fd corruption or a logIndex/log
// divergence bug — the caller (rollup) surfaces them as a hard error.
func readRecordAt(rf io.ReaderAt, logPos uint64, payloadLen uint32) (uint64, []byte, error) {
	if payloadLen > maxRecordPayload {
		return 0, nil, fmt.Errorf("append log: payloadLen %d exceeds %d cap at logPos=%d",
			payloadLen, maxRecordPayload, logPos)
	}
	total := uint64(recordFrameOverhead) + uint64(payloadLen)
	buf := make([]byte, total)
	if _, err := rf.ReadAt(buf, int64(logPos)); err != nil {
		return 0, nil, fmt.Errorf("append log: pread at %d (len=%d): %w", logPos, total, err)
	}
	declaredLen := binary.LittleEndian.Uint32(buf[0:4])
	if declaredLen != payloadLen {
		return 0, nil, fmt.Errorf("append log: frame payload_len %d != logIndex %d at logPos=%d", declaredLen, payloadLen, logPos)
	}
	fileOffset := binary.LittleEndian.Uint64(buf[4:12])
	wantCRC := binary.LittleEndian.Uint32(buf[12:16])
	payload := buf[recordFrameOverhead:]
	var offBuf [8]byte
	binary.LittleEndian.PutUint64(offBuf[:], fileOffset)
	gotCRC := crc32.Update(0, crcTable, offBuf[:])
	gotCRC = crc32.Update(gotCRC, crcTable, payload)
	if gotCRC != wantCRC {
		return 0, nil, fmt.Errorf("append log: CRC mismatch at logPos=%d", logPos)
	}
	return fileOffset, payload, nil
}

// advanceRollupOffset updates the log header's rollup_offset field and
// fsyncs. The new rollup_offset [8..16) and the recomputed CRC [28..32) are
// written together in a SINGLE WriteAt(hdr[8:32]) followed by ONE fsync
// (#1411 Lever 2), collapsing the prior two-phase write-offset-fsync /
// write-crc-fsync sequence that cost two fsyncs per rolled-up file —
// a per-file tax that dominated the small-file rollup pipeline.
//
// Crash-safety relies on first-sector atomicity: the offset and CRC fields
// both live within the first 512-byte sector, which storage devices persist
// atomically, so the combined write lands either fully or not at all and the
// header moves old-consistent → new-consistent as a unit. Any torn outcome
// (offset and CRC disagreeing) fails CRC validation on next boot
// (ErrLogBadHeaderCRC) and routes to the existing header re-init path —
// identical recovery posture to the old two-phase protocol. The difference
// from FIX-6: that protocol made no sector-atomicity assumption (it ordered
// the offset fsync strictly before the CRC fsync); this one does. A 24-byte
// write wholly inside the first sector is the standard atomic-sector case,
// so the assumption holds on conventional block devices.
//
// Idempotent: calling with the same newOffset twice is a no-op on disk
// state; monotone rollup_offset is enforced by the caller — this helper does
// not reject backward moves.
func advanceRollupOffset(f *os.File, newOffset uint64) error {
	// Read the existing header (need bytes [0..28) to recompute CRC).
	var hdr [logHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return fmt.Errorf("append log: read header for advance: %w", err)
	}

	// Write the new rollup_offset AND its recomputed CRC together, then fsync
	// ONCE (#1411 Lever 2). The offset field [8..16) and the CRC field
	// [28..32) both live within the first 512-byte sector, which storage
	// devices persist atomically, so a single fsync moves the header from the
	// old consistent state to the new consistent state as a unit. A crash
	// mid-fsync leaves either the OLD header (neither field reached the
	// platter) or the NEW header (both did); the only torn outcome — offset
	// and CRC disagreeing — fails CRC validation on next boot
	// (ErrLogBadHeaderCRC) and routes to the existing header re-init path.
	// The previous two-fsync write-offset-fsync-then-write-crc-fsync protocol
	// gave the IDENTICAL crash semantics (its intermediate new-offset/old-CRC
	// window also failed CRC) at twice the fsync cost — a per-rolled-up-file
	// tax that dominated the small-file rollup→S3 pipeline.
	binary.LittleEndian.PutUint64(hdr[8:16], newOffset)
	crc := crc32.Checksum(hdr[0:28], crcTable)
	binary.LittleEndian.PutUint32(hdr[28:32], crc)
	if _, err := f.WriteAt(hdr[8:32], 8); err != nil {
		return fmt.Errorf("append log: write header offset+crc: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("append log: fsync header: %w", err)
	}
	return nil
}

// initLogFile creates a new log file at path with a fresh header and
// fsyncs it for durability. The file is opened O_EXCL — initLogFile fails
// if path already exists.
//
// The initial rollup_offset is logHeaderSize (64) because rollup_offset is
// a byte offset within the file and the first record starts immediately
// after the header.
//
// Returns the opened *os.File (positioned at offset 0, caller must Seek or
// use pwrite/WriteAt for subsequent writes).
func initLogFile(path string, createdAt int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0644)
	if err != nil {
		return nil, fmt.Errorf("append log: create: %w", err)
	}
	h := logHeader{
		Magic:        logMagic,
		Version:      logVersion,
		RollupOffset: logHeaderSize,
		CreatedAt:    createdAt,
	}
	buf := marshalHeader(h)
	if _, err := f.WriteAt(buf[:], 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("append log: write initial header: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("append log: fsync initial: %w", err)
	}
	return f, nil
}

// readLogHeader reads and validates the header of an already-open log
// file. Returns the parsed header or ErrLogBad* on mismatch.
func readLogHeader(f *os.File) (logHeader, error) {
	var hdr [logHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return logHeader{}, fmt.Errorf("append log: read header: %w", err)
	}
	return unmarshalHeader(hdr[:])
}
