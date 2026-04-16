package destination

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// D-05 streaming envelope wire format.
//
//	[magic u32 BE | version u8 | frame_size u32 BE]                   ← 9-byte header
//	Repeated per frame:
//	[nonce 12B | ct_len u32 BE | ciphertext (plaintext||tag 16B)]
//	   aad = 8-byte counter big-endian || "data" (non-final) or "final"
//
// The counter-in-AAD binds every frame to its ordinal position so a
// reorder or duplication produces a tag mismatch. The final-tagged last
// frame makes truncation detectable: a reader that hits EOF without
// having seen one returns ErrDecryptFailed.
const (
	// "DFS1" magic = 0x44465331 ('D'=0x44, 'F'=0x46, 'S'=0x53, '1'=0x31).
	// Identifies the destination-envelope wire format and distinguishes it
	// from the "MDFS" memory-backup magic in pkg/metadata/store/memory.
	envelopeMagic     uint32 = 0x44465331
	envelopeVersion   uint8  = 1
	envelopeHeaderLen        = 4 + 1 + 4 // magic + version + frame_size
	// defaultFrameSize is the D-05 canonical choice (4 MiB). Selecting 0 at
	// construction time opts into this default.
	defaultFrameSize = 4 * 1024 * 1024
	gcmNonceSize     = 12
	gcmTagSize       = 16
	// maxFrameSize is a sanity cap enforced when parsing an untrusted
	// header. 64 MiB is well above the 4 MiB default and still bounded so
	// a tampered frame_size field cannot trigger a huge allocation
	// (T-03-10 DoS mitigation).
	maxFrameSize = 64 * 1024 * 1024
)

// aadDataTag / aadFinalTag are the ASCII suffixes that bind a frame's AAD
// to its position in the stream: every frame but the last carries "data";
// the terminator carries "final".
var (
	aadDataTag  = []byte("data")
	aadFinalTag = []byte("final")
)

// buildAAD returns the D-05 AAD for the given frame counter and tag:
// 8-byte big-endian counter || tag ("data" or "final"). Both seal and
// open sides use this so an AAD layout change lands in one place.
func buildAAD(counter uint64, tag []byte) []byte {
	aad := make([]byte, 8+len(tag))
	binary.BigEndian.PutUint64(aad[0:8], counter)
	copy(aad[8:], tag)
	return aad
}

// NewEncryptWriter returns a writer that encrypts plaintext into the D-05
// envelope wire format and forwards ciphertext frames to w. Close() MUST
// be called before the encoded stream is complete — it emits the
// final-tagged frame which the reader requires for truncation-resistance.
//
// The key is consumed by cipher.NewGCM during construction; the caller
// MAY zero it immediately after this function returns (D-09).
//
// frameSize=0 selects the 4 MiB default. Non-zero values override it for
// tests; production callers should pass 0.
func NewEncryptWriter(w io.Writer, key []byte, frameSize int) (io.WriteCloser, error) {
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("%w: key must be %d bytes, got %d", ErrInvalidKeyMaterial, aes256KeyLen, len(key))
	}
	if frameSize <= 0 {
		frameSize = defaultFrameSize
	}
	if frameSize > maxFrameSize {
		return nil, fmt.Errorf("%w: frame size %d exceeds cap %d", ErrIncompatibleConfig, frameSize, maxFrameSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: create AES cipher: %v", ErrInvalidKeyMaterial, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: create GCM: %v", ErrInvalidKeyMaterial, err)
	}

	// Header is static; emit it now so readers can fail fast on a
	// truncated stream without having consumed any frame bytes.
	hdr := make([]byte, envelopeHeaderLen)
	binary.BigEndian.PutUint32(hdr[0:4], envelopeMagic)
	hdr[4] = envelopeVersion
	binary.BigEndian.PutUint32(hdr[5:9], uint32(frameSize))
	if _, err := w.Write(hdr); err != nil {
		return nil, fmt.Errorf("write envelope header: %w", err)
	}

	return &encryptWriter{
		w:         w,
		aead:      gcm,
		frameSize: frameSize,
		buf:       make([]byte, 0, frameSize),
	}, nil
}

// encryptWriter buffers plaintext until frameSize is reached, then seals
// one frame with a random nonce, counter-in-AAD, and "data" / "final" tag.
type encryptWriter struct {
	w         io.Writer
	aead      cipher.AEAD
	buf       []byte // plaintext accumulator, capped at frameSize
	counter   uint64
	frameSize int
	err       error // sticky
	closed    bool
}

func (e *encryptWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, io.ErrClosedPipe
	}
	if e.err != nil {
		return 0, e.err
	}
	written := 0
	for len(p) > 0 {
		room := e.frameSize - len(e.buf)
		if room <= 0 {
			if err := e.emitFrame(false); err != nil {
				e.err = err
				return written, err
			}
			continue
		}
		n := len(p)
		if n > room {
			n = room
		}
		e.buf = append(e.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(e.buf) == e.frameSize {
			if err := e.emitFrame(false); err != nil {
				e.err = err
				return written, err
			}
		}
	}
	return written, nil
}

// Close emits the remaining buffered plaintext (possibly zero bytes) as a
// final-tagged frame. Subsequent Close calls are a no-op; subsequent
// Write calls return io.ErrClosedPipe.
func (e *encryptWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if e.err != nil {
		return e.err
	}
	// A zero-byte final frame is still required — that emptiness is what
	// tells the reader "no more data" rather than "stream got cut off".
	if err := e.emitFrame(true); err != nil {
		e.err = err
		return err
	}
	return nil
}

// emitFrame seals e.buf under a fresh random nonce with AAD encoding the
// frame counter and a "data"/"final" tag, writes the frame on the wire,
// advances the counter, and resets the buffer.
func (e *encryptWriter) emitFrame(final bool) error {
	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	tag := aadDataTag
	if final {
		tag = aadFinalTag
	}
	ct := e.aead.Seal(nil, nonce, e.buf, buildAAD(e.counter, tag))

	// Wire layout: nonce(12) | ct_len(4 BE) | ciphertext+tag
	hdr := make([]byte, gcmNonceSize+4)
	copy(hdr[0:gcmNonceSize], nonce)
	binary.BigEndian.PutUint32(hdr[gcmNonceSize:gcmNonceSize+4], uint32(len(ct)))
	if _, err := e.w.Write(hdr); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if _, err := e.w.Write(ct); err != nil {
		return fmt.Errorf("write frame body: %w", err)
	}

	e.counter++
	e.buf = e.buf[:0]
	return nil
}

// NewDecryptReader parses the D-05 envelope header from r and returns a
// reader that streams decrypted plaintext. A reader that reaches EOF
// without having seen a final-tagged frame returns ErrDecryptFailed on
// the next Read call.
//
// Key contract matches NewEncryptWriter: consumed by cipher.NewGCM, safe
// for the caller to zero immediately after this returns.
func NewDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("%w: key must be %d bytes, got %d", ErrInvalidKeyMaterial, aes256KeyLen, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: create AES cipher: %v", ErrInvalidKeyMaterial, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: create GCM: %v", ErrInvalidKeyMaterial, err)
	}

	hdr := make([]byte, envelopeHeaderLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("%w: read envelope header: %v", ErrDecryptFailed, err)
	}
	magic := binary.BigEndian.Uint32(hdr[0:4])
	if magic != envelopeMagic {
		return nil, fmt.Errorf("%w: bad magic 0x%08x (want 0x%08x)", ErrDecryptFailed, magic, envelopeMagic)
	}
	ver := hdr[4]
	if ver != envelopeVersion {
		return nil, fmt.Errorf("%w: unsupported envelope version %d", ErrDecryptFailed, ver)
	}
	frameSize := binary.BigEndian.Uint32(hdr[5:9])
	if frameSize == 0 || frameSize > maxFrameSize {
		return nil, fmt.Errorf("%w: frame size %d out of range (1..%d)", ErrDecryptFailed, frameSize, maxFrameSize)
	}

	return &decryptReader{
		r:         r,
		aead:      gcm,
		frameSize: int(frameSize),
	}, nil
}

// decryptReader reads one frame at a time, decrypts it, and serves bytes
// out of a pending slice until the caller drains it.
type decryptReader struct {
	r         io.Reader
	aead      cipher.AEAD
	pending   []byte // decrypted plaintext of current frame, not yet returned
	counter   uint64
	frameSize int
	sawFinal  bool
	eof       bool
	err       error // sticky
}

func (d *decryptReader) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	for len(d.pending) == 0 {
		if d.eof {
			return 0, io.EOF
		}
		if err := d.nextFrame(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.pending)
	d.pending = d.pending[n:]
	return n, nil
}

// nextFrame reads nonce+ct_len+ciphertext, decrypts under the expected
// counter+tag AAD, advances state, and latches eof when a final-tagged
// frame is consumed.
func (d *decryptReader) nextFrame() error {
	if d.sawFinal {
		// We already consumed the terminator; any subsequent bytes are
		// unexpected. Transition cleanly to EOF on next Read.
		d.eof = true
		return nil
	}

	hdr := make([]byte, gcmNonceSize+4)
	if _, err := io.ReadFull(d.r, hdr); err != nil {
		// EOF with no final-tagged frame = truncation.
		return fmt.Errorf("%w: read frame header (counter=%d): %v", ErrDecryptFailed, d.counter, err)
	}
	nonce := hdr[0:gcmNonceSize]
	ctLen := binary.BigEndian.Uint32(hdr[gcmNonceSize : gcmNonceSize+4])

	// A valid frame's ciphertext is plaintext_len + gcmTagSize; plaintext
	// cannot exceed frameSize. Reject absurd ct_len before allocating.
	if ctLen < gcmTagSize || int(ctLen) > d.frameSize+gcmTagSize {
		return fmt.Errorf("%w: frame ct_len %d out of range", ErrDecryptFailed, ctLen)
	}

	ct := make([]byte, ctLen)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return fmt.Errorf("%w: read frame body (counter=%d): %v", ErrDecryptFailed, d.counter, err)
	}

	// Try the "data" AAD first. If Open fails, retry with "final" — the
	// terminator is otherwise indistinguishable on the wire. This means
	// both tampering and a premature final-tag behave identically: they
	// surface as ErrDecryptFailed, which is exactly the D-05 contract
	// (the errors MUST be indistinguishable by design, D-07).
	pt, err := d.aead.Open(nil, nonce, ct, buildAAD(d.counter, aadDataTag))
	if err != nil {
		pt, err = d.aead.Open(nil, nonce, ct, buildAAD(d.counter, aadFinalTag))
		if err != nil {
			return fmt.Errorf("%w: frame %d AEAD open", ErrDecryptFailed, d.counter)
		}
		d.sawFinal = true
	}

	d.pending = pt
	d.counter++

	// If this was the final frame with zero-length plaintext, nextFrame
	// returns with pending==[] and sawFinal==true. The Read loop will
	// call nextFrame again, which sets eof and returns nil, then the
	// outer loop sees eof and surfaces io.EOF. No additional bookkeeping
	// required.
	return nil
}
