// Package backup provides the shared binary envelope format for DittoFS
// metadata backup streams. Engine-specific drivers (badger, memory,
// postgres) wrap their opaque payload inside this envelope, which
// provides magic-byte identification, versioning, engine tag routing,
// and trailing CRC32 integrity checking.
//
// Wire format:
//
//	magic        [4]byte   "DFBK"
//	version      uint32 LE (1 = current)
//	engine_len   uint16 LE
//	engine_tag   [engine_len]byte  (e.g. "badger", "memory", "postgres")
//	payload      [...]byte         (engine-specific, variable length)
//	crc32c       uint32 LE         (Castagnoli, over everything before this field)
//
// The envelope does NOT import pkg/metadata to avoid import cycles.
package backup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// envelopeMagic is the 4-byte prefix of every DittoFS backup stream.
// Chosen to avoid collision with the append-log magic "DFLG".
var envelopeMagic = [4]byte{'D', 'F', 'B', 'K'}

// crcTable is the Castagnoli CRC32 table. Hardware-accelerated on
// amd64 (SSE4.2) and arm64 (ARMv8 CRC32 extension).
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// EnvelopeVersion is the current envelope format version.
const EnvelopeVersion = uint32(1)

// Envelope-level error sentinels.
var (
	// ErrBadMagic is returned when the first 4 bytes are not "DFBK".
	ErrBadMagic = errors.New("backup: invalid envelope magic")

	// ErrUnsupportedVersion is returned when the envelope version field
	// is not recognized by this build.
	ErrUnsupportedVersion = errors.New("backup: unsupported envelope version")

	// ErrEngineMismatch is returned by VerifyEngine when the stream's
	// engine tag does not match the expected engine.
	ErrEngineMismatch = errors.New("backup: engine tag mismatch")

	// ErrTruncated is returned when the stream ends before the header
	// or trailing CRC can be fully read.
	ErrTruncated = errors.New("backup: envelope truncated")

	// ErrCRCMismatch is returned when the trailing CRC32 does not match
	// the computed checksum over the preceding bytes.
	ErrCRCMismatch = errors.New("backup: CRC32 verification failed")
)

// --------------------------------------------------------------------------
// Writer
// --------------------------------------------------------------------------

// Writer wraps an io.Writer and accumulates a running CRC32 (Castagnoli)
// over all bytes written through it. After the payload is complete, call
// Finish to append the trailing 4-byte CRC.
type Writer struct {
	w   io.Writer
	crc hash.Hash32
}

// NewWriter writes the envelope header (magic, version, engine tag) to w
// and returns a Writer that accumulates CRC over all subsequent writes.
// The engine tag is included in the CRC.
func NewWriter(w io.Writer, engineTag string) (*Writer, error) {
	if len(engineTag) > 65535 {
		return nil, fmt.Errorf("backup: engine tag too long (%d bytes, max 65535)", len(engineTag))
	}

	crc := crc32.New(crcTable)
	mw := io.MultiWriter(w, crc)

	// magic
	if _, err := mw.Write(envelopeMagic[:]); err != nil {
		return nil, fmt.Errorf("backup: write magic: %w", err)
	}

	// version (uint32 LE)
	var vBuf [4]byte
	binary.LittleEndian.PutUint32(vBuf[:], EnvelopeVersion)
	if _, err := mw.Write(vBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write version: %w", err)
	}

	// engine_len (uint16 LE) + engine_tag
	var eBuf [2]byte
	binary.LittleEndian.PutUint16(eBuf[:], uint16(len(engineTag)))
	if _, err := mw.Write(eBuf[:]); err != nil {
		return nil, fmt.Errorf("backup: write engine_len: %w", err)
	}
	if _, err := mw.Write([]byte(engineTag)); err != nil {
		return nil, fmt.Errorf("backup: write engine_tag: %w", err)
	}

	return &Writer{w: w, crc: crc}, nil
}

// Write writes p to the underlying writer and accumulates CRC.
func (ew *Writer) Write(p []byte) (int, error) {
	// Write to CRC accumulator.
	ew.crc.Write(p)
	// Write to underlying writer.
	return ew.w.Write(p)
}

// Finish writes the trailing 4-byte CRC32 (Castagnoli) to the
// underlying writer. The CRC covers everything written since NewWriter
// (header + all Write calls). Finish must be called exactly once.
func (ew *Writer) Finish() error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], ew.crc.Sum32())
	_, err := ew.w.Write(buf[:])
	return err
}

// --------------------------------------------------------------------------
// Reader
// --------------------------------------------------------------------------

// ReadHeader reads and validates the envelope header from r. On success
// it returns the engine tag and an io.Reader that accumulates CRC over
// all subsequent reads (the payload). The caller must use the returned
// reader for payload reads and then call VerifyCRC with the accumulated
// hash when the payload is exhausted.
//
// The returned hash.Hash32 is the CRC accumulator initialized with the
// header bytes. Pass it to VerifyCRC after draining the payload reader.
func ReadHeader(r io.Reader) (engineTag string, payloadReader io.Reader, accumulated hash.Hash32, err error) {
	crc := crc32.New(crcTable)
	tr := io.TeeReader(r, crc)

	// magic
	var magic [4]byte
	if _, err := io.ReadFull(tr, magic[:]); err != nil {
		return "", nil, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	if magic != envelopeMagic {
		return "", nil, nil, fmt.Errorf("%w: got %x", ErrBadMagic, magic)
	}

	// version
	var vBuf [4]byte
	if _, err := io.ReadFull(tr, vBuf[:]); err != nil {
		return "", nil, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	version := binary.LittleEndian.Uint32(vBuf[:])
	if version != EnvelopeVersion {
		return "", nil, nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, EnvelopeVersion)
	}

	// engine_len
	var eBuf [2]byte
	if _, err := io.ReadFull(tr, eBuf[:]); err != nil {
		return "", nil, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	eLen := binary.LittleEndian.Uint16(eBuf[:])

	// engine_tag
	tag := make([]byte, eLen)
	if _, err := io.ReadFull(tr, tag); err != nil {
		return "", nil, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
	}

	// Return a tee reader that continues accumulating CRC for the payload.
	return string(tag), io.TeeReader(r, crc), crc, nil
}

// VerifyCRC reads the trailing 4-byte CRC32 from r and compares it
// against the accumulated checksum. Returns ErrCRCMismatch on mismatch
// or ErrTruncated if the trailing bytes cannot be read.
//
// r must be the ORIGINAL reader (not the tee reader) so the trailing
// CRC bytes are not fed back into the accumulator.
func VerifyCRC(r io.Reader, accumulated hash.Hash32) error {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	expected := binary.LittleEndian.Uint32(buf[:])
	got := accumulated.Sum32()
	if got != expected {
		return fmt.Errorf("%w: got %08x, want %08x", ErrCRCMismatch, got, expected)
	}
	return nil
}

// VerifyEngine checks that the stream's engine tag matches the expected
// tag. Returns ErrEngineMismatch wrapping both tags if they differ.
func VerifyEngine(got, want string) error {
	if got != want {
		return fmt.Errorf("%w: stream has %q, expected %q", ErrEngineMismatch, got, want)
	}
	return nil
}
