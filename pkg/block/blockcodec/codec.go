// Package blockcodec implements the DittoFS block wire format: a streaming
// framing layer that packs one or more chunk records into a single block object.
//
// # Wire format
//
//	Block = [ preamble ][ record_0 ][ record_1 ] … [ record_{N-1} ]
//
//	preamble:
//	  magic      [4]byte = {'D','F','B','1'}
//	  flags      uint8            // bit0 = 1 → record headers are AEAD-sealed
//	  blockID    uvarint(len) + len bytes (UTF-8)
//
//	record (plaintext, flags bit0 = 0):
//	  hash       [32]byte         // chunk BLAKE3 content hash
//	  wireLen    uvarint
//	  wire       [wireLen]byte    // enc(comp(chunk)) — already-transformed body
//
//	record (sealed, flags bit0 = 1):
//	  sealedHdrLen uvarint
//	  sealedHdr    [sealedHdrLen]byte   // AEAD seal of {hash[32], wireLen uvarint}
//	                                    // AAD = blockID||recordIndex(uvarint)
//	  wire         [wireLen]byte        // wireLen recovered by opening sealedHdr
package blockcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
)

// magic is the 4-byte preamble for DittoFS block objects.
var magic = [4]byte{'D', 'F', 'B', '1'}

const flagSealed = uint8(1 << 0) // bit0: record headers are AEAD-sealed

// Sealer seals and opens record sub-headers for encrypted shares.
// A nil Sealer means plaintext framing with no header encryption.
type Sealer interface {
	// Seal encrypts plaintextHdr using aad as additional authenticated data.
	Seal(plaintextHdr, aad []byte) ([]byte, error)
	// Open decrypts and authenticates sealedHdr.
	Open(sealedHdr, aad []byte) ([]byte, error)
}

// Builder streams one block to an underlying writer. Bodies are never held
// entirely in RAM — each call to Add writes immediately.
type Builder struct {
	w           io.Writer
	blockID     string
	sealer      Sealer
	written     int64
	recordIndex uint64
}

// NewBuilder writes the block preamble to w and returns a streaming Builder.
// sealer == nil produces plaintext record framing; a non-nil Sealer encrypts
// the {hash, wireLen} sub-header of every record (the body remains plaintext).
func NewBuilder(w io.Writer, blockID string, sealer Sealer) (*Builder, error) {
	b := &Builder{
		w:       w,
		blockID: blockID,
		sealer:  sealer,
	}
	if err := b.writePreamble(); err != nil {
		return nil, fmt.Errorf("blockcodec: write preamble: %w", err)
	}
	return b, nil
}

// Add frames one record (header + body) and returns a ChunkLocator whose
// Offset and Length address the wire body within the block object being built.
func (b *Builder) Add(hash block.ContentHash, wire []byte) (block.ChunkLocator, error) {
	if b.sealer != nil {
		return b.addSealed(hash, wire)
	}
	return b.addPlaintext(hash, wire)
}

// Finish validates the block state and returns the total number of bytes
// written to the underlying writer.
func (b *Builder) Finish() (int64, error) {
	return b.written, nil
}

// Record is one entry in a parsed block, carrying the chunk hash and the
// byte range of its wire body within the block object. It is the codec's
// index-rebuild output — callers convert it to a block.ChunkLocator by
// supplying the block's object key.
type Record struct {
	Hash       block.ContentHash
	WireOffset int64 // byte offset of the wire body within the block object
	WireLength int64 // byte length of the wire body
}

// Parse reads a complete block from data, decodes the preamble and every
// record, and returns the block ID and an ordered slice of Records. sealer
// must be non-nil when the block was written with a Sealer; nil for plaintext
// blocks.
func Parse(data []byte, sealer Sealer) (blockID string, records []Record, err error) {
	r := &cursor{buf: data}

	// preamble: magic
	m, err := r.readN(4)
	if err != nil {
		return "", nil, fmt.Errorf("blockcodec: read magic: %w", err)
	}
	if m[0] != magic[0] || m[1] != magic[1] || m[2] != magic[2] || m[3] != magic[3] {
		return "", nil, fmt.Errorf("blockcodec: invalid magic bytes %v", m)
	}

	// preamble: flags
	flags, err := r.readByte()
	if err != nil {
		return "", nil, fmt.Errorf("blockcodec: read flags: %w", err)
	}

	// preamble: blockID
	idBytes, err := r.readVarBytes()
	if err != nil {
		return "", nil, fmt.Errorf("blockcodec: read blockID: %w", err)
	}
	blockID = string(idBytes)

	sealed := flags&flagSealed != 0
	if sealed && sealer == nil {
		return "", nil, errors.New("blockcodec: block is sealed but no Sealer provided")
	}
	if !sealed {
		sealer = nil // belt-and-suspenders: don't use a sealer on a plaintext block
	}

	var idx uint64
	for r.remaining() > 0 {
		var rec Record
		if sealer == nil {
			// Plaintext record: hash[32] + wireLen uvarint + wire[wireLen]
			hashBytes, err := r.readN(32)
			if err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: read hash: %w", idx, err)
			}
			copy(rec.Hash[:], hashBytes)

			wireLen, err := r.readUvarint()
			if err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: read wireLen: %w", idx, err)
			}

			rec.WireOffset = int64(r.pos)
			if _, err := r.readN(int(wireLen)); err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: read wire body: %w", idx, err)
			}
			rec.WireLength = int64(wireLen)
		} else {
			// Sealed record: sealedHdrLen uvarint + sealedHdr + wire[wireLen]
			sealedHdr, err := r.readVarBytes()
			if err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: read sealed header: %w", idx, err)
			}

			aad := buildAAD(blockID, idx)
			plaintextHdr, err := sealer.Open(sealedHdr, aad)
			if err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: open sealed header: %w", idx, err)
			}
			if len(plaintextHdr) < 32 {
				return "", nil, fmt.Errorf("blockcodec: record %d: sealed header plaintext too short (%d bytes)", idx, len(plaintextHdr))
			}
			copy(rec.Hash[:], plaintextHdr[:32])

			wireLen, n := binary.Uvarint(plaintextHdr[32:])
			if n <= 0 {
				return "", nil, fmt.Errorf("blockcodec: record %d: decode wireLen from sealed header (n=%d)", idx, n)
			}

			rec.WireOffset = int64(r.pos)
			if _, err := r.readN(int(wireLen)); err != nil {
				return "", nil, fmt.Errorf("blockcodec: record %d: read wire body: %w", idx, err)
			}
			rec.WireLength = int64(wireLen)
		}
		records = append(records, rec)
		idx++
	}

	return blockID, records, nil
}

// ---- Builder helpers --------------------------------------------------------

func (b *Builder) writePreamble() error {
	// magic [4]byte
	if err := b.writeRaw(magic[:]); err != nil {
		return err
	}
	// flags uint8
	flags := uint8(0)
	if b.sealer != nil {
		flags |= flagSealed
	}
	if err := b.writeByte(flags); err != nil {
		return err
	}
	// blockID: uvarint(len) + bytes
	return b.writeVarBytes([]byte(b.blockID))
}

func (b *Builder) addPlaintext(hash block.ContentHash, wire []byte) (block.ChunkLocator, error) {
	// hash [32]byte
	if err := b.writeRaw(hash[:]); err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: write hash: %w", err)
	}
	// wireLen uvarint
	if err := b.writeUvarint(uint64(len(wire))); err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: write wireLen: %w", err)
	}
	// body
	wireBodyStart := b.written
	if err := b.writeRaw(wire); err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: write wire body: %w", err)
	}
	b.recordIndex++
	return block.ChunkLocator{
		BlockID: b.blockID,
		Offset:  wireBodyStart,
		Length:  int64(len(wire)),
	}, nil
}

func (b *Builder) addSealed(hash block.ContentHash, wire []byte) (block.ChunkLocator, error) {
	// Build plaintext header: hash[32] + wireLen uvarint
	var hdrBuf bytes.Buffer
	hdrBuf.Write(hash[:])
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(wire)))
	hdrBuf.Write(tmp[:n])

	aad := buildAAD(b.blockID, b.recordIndex)
	sealedHdr, err := b.sealer.Seal(hdrBuf.Bytes(), aad)
	if err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: seal header: %w", err)
	}

	// sealedHdrLen uvarint + sealedHdr bytes
	if err := b.writeVarBytes(sealedHdr); err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: write sealed header: %w", err)
	}

	// body (plaintext-visible position in the block)
	wireBodyStart := b.written
	if err := b.writeRaw(wire); err != nil {
		return block.ChunkLocator{}, fmt.Errorf("blockcodec: write wire body: %w", err)
	}
	b.recordIndex++
	return block.ChunkLocator{
		BlockID: b.blockID,
		Offset:  wireBodyStart,
		Length:  int64(len(wire)),
	}, nil
}

func (b *Builder) writeByte(v byte) error {
	return b.writeRaw([]byte{v})
}

func (b *Builder) writeRaw(p []byte) error {
	n, err := b.w.Write(p)
	b.written += int64(n)
	return err
}

func (b *Builder) writeUvarint(v uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return b.writeRaw(buf[:n])
}

func (b *Builder) writeVarBytes(data []byte) error {
	if err := b.writeUvarint(uint64(len(data))); err != nil {
		return err
	}
	return b.writeRaw(data)
}

// buildAAD constructs the additional authenticated data for a sealed record:
// blockID bytes concatenated with the record index encoded as a uvarint.
func buildAAD(blockID string, recordIndex uint64) []byte {
	idBytes := []byte(blockID)
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], recordIndex)
	aad := make([]byte, len(idBytes)+n)
	copy(aad, idBytes)
	copy(aad[len(idBytes):], tmp[:n])
	return aad
}

// ---- cursor: read-only view over a byte slice --------------------------------

type cursor struct {
	buf []byte
	pos int
}

func (c *cursor) remaining() int { return len(c.buf) - c.pos }

func (c *cursor) readN(n int) ([]byte, error) {
	if c.remaining() < n {
		return nil, fmt.Errorf("unexpected EOF: need %d bytes, have %d", n, c.remaining())
	}
	data := c.buf[c.pos : c.pos+n]
	c.pos += n
	return data, nil
}

func (c *cursor) readByte() (byte, error) {
	b, err := c.readN(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) readUvarint() (uint64, error) {
	v, n := binary.Uvarint(c.buf[c.pos:])
	if n == 0 {
		return 0, errors.New("buffer too small for uvarint")
	}
	if n < 0 {
		return 0, errors.New("uvarint value overflows uint64")
	}
	c.pos += n
	return v, nil
}

func (c *cursor) readVarBytes() ([]byte, error) {
	l, err := c.readUvarint()
	if err != nil {
		return nil, err
	}
	return c.readN(int(l))
}
