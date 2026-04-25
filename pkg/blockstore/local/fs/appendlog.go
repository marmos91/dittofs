package fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// Append-log on-disk format constants. See D-09, D-10, D-11 in
// .planning/phases/10-fastcdc-chunker-hybrid-local-store-a1/10-CONTEXT.md.
//
// Log layout:
//
//	[0..64)                   fixed header (see logHeader + marshalHeader)
//	[64..)                    append-only stream of records (see writeRecord)
//
// Record framing:
//
//	uint32 LE  payload_len
//	uint64 LE  file_offset    (byte offset inside the logical payload)
//	uint32 LE  crc32c         (Castagnoli, over file_offset_8B_LE || payload)
//	[]byte     payload        (payload_len bytes)
//
// Recovery (LSL-06) truncates at the first bad CRC or short record.
const (
	logHeaderSize       = 64
	recordFrameOverhead = 16 // payload_len(4) + file_offset(8) + crc(4)
	logVersion          = uint32(1)
)

// logMagic is the fixed 4-byte prefix of every DittoFS append log: 'DFLG'.
var logMagic = [4]byte{'D', 'F', 'L', 'G'}

// crcTable is the Castagnoli CRC32 table used for record and header CRCs
// (D-10). Hardware-accelerated on amd64 (SSE4.2) and arm64 (ARMv8 CRC32
// extension).
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// logHeader is the 64-byte on-disk layout documented in D-09.
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

// writeRecord writes one framed record to w. The on-disk layout (D-11) is
// payload_len(uint32 LE) || file_offset(uint64 LE) || crc(uint32 LE) || payload.
// The CRC is computed over (file_offset_8B_LE || payload).
//
// Returns the number of bytes written (including both the 16-byte frame
// header and the payload).
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
// Return semantics:
//   - (off, payload, true, nil) on success.
//   - (0, nil, false, nil) on clean EOF, short read, length-overflow, or CRC
//     mismatch — caller truncates the log at the previous valid position
//     (LSL-06).
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
	// FIX-4: clamp the per-record payload allocation. A torn-tail or
	// hostile log frame can present an arbitrary 32-bit payload_len;
	// without a cap, recovery (or any rollup replay) would attempt a
	// 4 GiB-1 allocation per bad frame → OOM / DoS. The cap is set just
	// above the chunker's hard maximum (16 MiB) plus slack so legitimate
	// records always pass; anything larger is treated as corruption and
	// the caller (LSL-06) truncates the log at the previous valid
	// position.
	const maxRecordPayload = 17 * 1024 * 1024
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

// advanceRollupOffset atomically updates the log header's rollup_offset
// field and fsyncs. FIX-6: split into a two-phase pwrite+fsync sequence
// so the on-disk state is always either fully old-valid, fully new-valid,
// or "new-offset present but CRC stale" (which recovery treats as a hard
// header corruption → re-init, the same posture as any other bad-CRC
// header). The previous single 32-byte WriteAt could yield a torn
// rollup_offset whose lower bytes were updated but upper bytes weren't,
// while the CRC happened to land on disk first and validate against the
// torn value — silently corrupting recovery's view of the rollup point.
//
// Phase 1: pwrite new rollup_offset bytes [8..16), fsync.
// Phase 2: recompute CRC over [0..28), pwrite CRC bytes [28..32), fsync.
//
// Idempotent: calling with the same newOffset twice is a no-op on disk
// state (D-12 step 4; INV-03 monotone rollup_offset is enforced by the
// caller — this helper does not reject backward moves).
func advanceRollupOffset(f *os.File, newOffset uint64) error {
	// Read the existing header (need bytes [0..28) to recompute CRC).
	var hdr [logHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return fmt.Errorf("append log: read header for advance: %w", err)
	}

	// Phase 1: write the new rollup_offset bytes, fsync. After this point
	// a crash leaves either the OLD rollup_offset on disk (write didn't
	// reach the platter) or the NEW rollup_offset with the OLD CRC. The
	// latter fails CRC validation on next boot — recovery treats it as
	// header corruption and re-inits the log (existing LSL-06 path).
	binary.LittleEndian.PutUint64(hdr[8:16], newOffset)
	if _, err := f.WriteAt(hdr[8:16], 8); err != nil {
		return fmt.Errorf("append log: write header rollup_offset: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("append log: fsync header phase 1: %w", err)
	}

	// Phase 2: recompute the CRC over [0..28) using the in-memory bytes
	// we just wrote, pwrite [28..32), fsync. After this completes the
	// header is fully valid. A crash between phase 1 and phase 2 is the
	// "stale CRC" case described above — safe.
	crc := crc32.Checksum(hdr[0:28], crcTable)
	binary.LittleEndian.PutUint32(hdr[28:32], crc)
	if _, err := f.WriteAt(hdr[28:32], 28); err != nil {
		return fmt.Errorf("append log: write header crc: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("append log: fsync header phase 2: %w", err)
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
