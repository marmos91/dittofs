package fs

// Read-only reconstruction of a pre-journal ("v0.26.0") local-only layout.
//
// A pre-journal local store kept two on-disk kinds: per-payload append logs
// (logs/<payloadID>.log) and packed content-addressed chunks (blobs/*.blob).
// The journal cannot read either, so opening such a directory as an empty
// journal would serve every file as zeros. For a REMOTE-backed share the
// authoritative bytes live in the remote and are re-materialized from the
// surviving manifest; for a LOCAL-ONLY share the on-disk bytes are the sole
// copy, and only the append logs are recoverable — the blob substrate stored
// bytes raw with no framing and was located solely through a hash->location
// index that is gone (the index table is dropped on upgrade and the blobs are
// unframed, so their contents cannot be resolved).
//
// This file therefore reconstructs LOCAL-ONLY payloads from the append logs
// alone, gated on a proof that the logs are complete: a rollup that moved
// records into blobs leaves the original records in the log until a compaction
// pass rewrites the log and sets LogFlagCompacted in its header. So if NO log
// is flagged compacted, every byte still lives in some log and the blobs are
// redundant. Any compacted log means some bytes live only in the unrecoverable
// blobs, and migration is refused (the guardrail keeps the bytes on disk).
//
// Everything here is READ-ONLY over the legacy bytes and self-contained (the
// framing constants are vendored, not shared) so the whole file can be deleted
// once the upgrade window closes.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	// legacyLogHeaderSize is the fixed append-log header length.
	legacyLogHeaderSize = 64
	// legacyRecordFrameOverhead is payload_len(4) + file_offset(8) + crc(4).
	legacyRecordFrameOverhead = 16
	// legacyLogVersion is the only append-log version ever written.
	legacyLogVersion = uint32(1)
	// legacyMaxRecordPayload clamps a per-record payload allocation so a torn
	// or hostile frame cannot drive a multi-GiB allocation. Matches the writer's
	// cap (chunker hard max 16 MiB + slack).
	legacyMaxRecordPayload = 17 * 1024 * 1024
	// legacyLogFlagCompacted marks a log whose rolled-up records were physically
	// dropped by a compaction pass. Its presence means some bytes live only in
	// the (unrecoverable) blob substrate.
	legacyLogFlagCompacted uint32 = 1 << 0
)

// legacyLogMagic is the 4-byte append-log prefix 'DFLG'.
var legacyLogMagic = [4]byte{'D', 'F', 'L', 'G'}

// legacyCRCTable is the Castagnoli CRC32 table used for record and header CRCs.
var legacyCRCTable = crc32.MakeTable(crc32.Castagnoli)

// legacyLogState classifies a candidate .log file's 64-byte header.
type legacyLogState int

const (
	legacyLogNotALog    legacyLogState = iota // bad magic: not one of our logs
	legacyLogOK                               // valid header, not compacted
	legacyLogCompacted                        // valid header with the compacted flag set
	legacyLogUnreadable                       // our magic but a torn/invalid header
)

// classifyLegacyLog reads and validates a log's 64-byte header. It never reads
// past the header, so it is cheap enough to run over every log during the gate.
func classifyLegacyLog(path string) (legacyLogState, error) {
	f, err := os.Open(path)
	if err != nil {
		return legacyLogNotALog, err
	}
	defer func() { _ = f.Close() }()

	var hdr [legacyLogHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Shorter than a header: cannot be a valid log we wrote.
			return legacyLogUnreadable, nil
		}
		return legacyLogNotALog, err
	}
	var magic [4]byte
	copy(magic[:], hdr[0:4])
	if magic != legacyLogMagic {
		return legacyLogNotALog, nil
	}
	if binary.LittleEndian.Uint32(hdr[4:8]) != legacyLogVersion {
		return legacyLogUnreadable, nil
	}
	wantCRC := binary.LittleEndian.Uint32(hdr[28:32])
	if crc32.Checksum(hdr[0:28], legacyCRCTable) != wantCRC {
		return legacyLogUnreadable, nil
	}
	flags := binary.LittleEndian.Uint32(hdr[16:20])
	if flags&legacyLogFlagCompacted != 0 {
		return legacyLogCompacted, nil
	}
	return legacyLogOK, nil
}

// firstUnmigratableLog walks logsDir for the first append log that blocks
// auto-migration and returns a human reason plus that log's path; it returns
// ("", "", nil) when every real log is complete and replayable. A compacted log
// means rolled-up bytes now live only in the unrecoverable blobs; a torn or
// invalid header means the log itself cannot be replayed. Either way the share
// must not be auto-migrated, and naming the two cases apart keeps the refusal
// from blaming "compaction" for a log that is merely corrupt.
func firstUnmigratableLog(logsDir string) (reason, offendingLog string, err error) {
	walkErr := filepath.WalkDir(logsDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".log") {
			return nil
		}
		state, cerr := classifyLegacyLog(path)
		if cerr != nil {
			return cerr
		}
		switch state {
		case legacyLogCompacted:
			reason, offendingLog = "an append log was compacted, so some bytes live only in unrecoverable blobs", path
			return filepath.SkipAll
		case legacyLogUnreadable:
			reason, offendingLog = "an append log is torn or has an invalid header, so it cannot be replayed", path
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", "", walkErr
	}
	return reason, offendingLog, nil
}

// scanLegacyPayloads walks logsDir and returns a map of payloadID -> absolute
// log path for every valid, uncompacted log. The payloadID is the log's path
// relative to logsDir with the .log suffix removed (pre-journal payloadIDs are
// path-keyed and may nest, e.g. logs/<share>/<file>.log), slash-normalized to
// match the keyspace the journal and metadata use.
func scanLegacyPayloads(logsDir string) (map[string]string, error) {
	payloads := make(map[string]string)
	walkErr := filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".log") {
			return nil
		}
		state, cerr := classifyLegacyLog(path)
		if cerr != nil {
			return cerr
		}
		if state != legacyLogOK {
			// legacyLogNotALog: stray file, skip. Compacted/unreadable can't
			// happen here (the gate refused before we ever scan), but skip them
			// defensively rather than reconstruct a partial payload.
			return nil
		}
		rel, rerr := filepath.Rel(logsDir, path)
		if rerr != nil {
			return rerr
		}
		payloadID := filepath.ToSlash(strings.TrimSuffix(rel, ".log"))
		payloads[payloadID] = path
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return payloads, nil
}

// replayLegacyLog reads logPath and calls emit(fileOffset, payload) for every
// intact record in log order. Reconstruction is "replay in order": applying
// records in arrival order reproduces last-write-wins (a later write at an
// offset supersedes an earlier one) and never touches unwritten holes, exactly
// as the writer intended. Reading stops at the first torn/short record or bad
// CRC (a crash-truncated tail), mirroring the writer's own recovery.
func replayLegacyLog(logPath string, emit func(fileOffset uint64, payload []byte) error) error {
	f, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(legacyLogHeaderSize, io.SeekStart); err != nil {
		return fmt.Errorf("legacy log %q: seek past header: %w", logPath, err)
	}
	r := bufio.NewReader(f)
	for {
		off, payload, ok, rerr := readLegacyRecord(r)
		if rerr != nil {
			return fmt.Errorf("legacy log %q: %w", logPath, rerr)
		}
		if !ok {
			return nil // clean EOF or crash-truncated tail
		}
		if err := emit(off, payload); err != nil {
			return err
		}
	}
}

// readLegacyRecord reads one framed record: payload_len(u32 LE) |
// file_offset(u64 LE) | crc(u32 LE) | payload. The CRC is Castagnoli over
// (file_offset_8LE || payload). Returns ok=false (no error) on clean EOF, a
// short/torn frame, an over-cap length, or a CRC mismatch — the caller treats
// all of those as end-of-log. Returns an error only on a hard I/O failure.
func readLegacyRecord(r io.Reader) (uint64, []byte, bool, error) {
	var frame [legacyRecordFrameOverhead]byte
	if _, err := io.ReadFull(r, frame[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("read frame: %w", err)
	}
	payloadLen := binary.LittleEndian.Uint32(frame[0:4])
	fileOffset := binary.LittleEndian.Uint64(frame[4:12])
	wantCRC := binary.LittleEndian.Uint32(frame[12:16])
	if payloadLen > legacyMaxRecordPayload {
		return 0, nil, false, nil
	}
	// The offset and end of the record must fit int64: replay hands the offset
	// to a WriteAt that takes an int64, and a wider value would wrap negative.
	// Such a record is unreplayable, so stop the log here like a torn tail.
	if fileOffset > math.MaxInt64-uint64(payloadLen) {
		return 0, nil, false, nil
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("read payload: %w", err)
	}
	var offBuf [8]byte
	binary.LittleEndian.PutUint64(offBuf[:], fileOffset)
	gotCRC := crc32.Update(0, legacyCRCTable, offBuf[:])
	gotCRC = crc32.Update(gotCRC, legacyCRCTable, payload)
	if gotCRC != wantCRC {
		return 0, nil, false, nil
	}
	return fileOffset, payload, true, nil
}
