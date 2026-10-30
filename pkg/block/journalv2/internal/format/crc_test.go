package format

import (
	"bytes"
	"testing"
)

// TestCRC32CMatchesTheRFC3720Vectors pins the checksum against the published
// CRC32C test vectors, so the codec does not check itself against itself.
func TestCRC32CMatchesTheRFC3720Vectors(t *testing.T) {
	inc, dec := make([]byte, 32), make([]byte, 32)
	for i := range 32 {
		inc[i], dec[i] = byte(i), byte(31-i)
	}

	for _, c := range []struct {
		name string
		in   []byte
		want uint32
	}{
		{"zeros", make([]byte, 32), 0x8A9136AA},
		{"ones", bytes.Repeat([]byte{0xff}, 32), 0x62A8AB43},
		{"incrementing", inc, 0x46DD794E},
		{"decrementing", dec, 0x113FDB5C},
		{"scsi read(10) PDU", bytes.Join([][]byte{
			{0x01, 0xc0, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x14, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x04, 0x00},
			{0x00, 0x00, 0x00, 0x14},
			{0x00, 0x00, 0x00, 0x18},
			{0x28, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
			{0x02, 0x00, 0x00, 0x00},
			{0x00, 0x00, 0x00, 0x00},
		}, nil), 0xD9963A56},
	} {
		if got := crc32c(c.in); got != c.want {
			t.Errorf("crc32c(%s) = 0x%08x, want 0x%08x", c.name, got, c.want)
		}
	}
}

// TestSeededCRC32CIsTheCRCOfTheIdentityThenTheBytes pins the seeded checksum
// against the standard library's CRC32C of the identity's bytes then the
// covered bytes, and against a hand-computed literal of the golden seal
// marker's span, so a seed that is ignored — or applied after the bytes
// rather than before — reads back wrong.
func TestSeededCRC32CIsTheCRCOfTheIdentityThenTheBytes(t *testing.T) {
	covered := readGolden(t, "seal")[:28]

	for _, c := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"the golden's first 28 bytes", covered},
		{"32 bytes of 0xff", bytes.Repeat([]byte{0xff}, 32)},
	} {
		if got, want := crc32cSeeded(identitySeed(testIdentity), c.in), wantSeededCRC(testIdentity, c.in); got != want {
			t.Errorf("crc32cSeeded(%s) = 0x%08x, want 0x%08x", c.name, got, want)
		}
	}

	// The golden's span seeded with testIdentity, computed by hand, pins
	// the whole codec: the same bytes unseeded give 0x62df0dba, so a seed
	// that is dropped or appended after the bytes reads back wrong here.
	if got, want := crc32cSeeded(identitySeed(testIdentity), covered), uint32(0x0b0bbe8b); got != want {
		t.Errorf("crc32cSeeded(the golden's first 28 bytes) = 0x%08x, want 0x%08x", got, want)
	}
	if got, want := crc32cSeeded(identitySeed(testIdentity), covered), crc32c(covered); got == want {
		t.Errorf("crc32cSeeded(the golden's first 28 bytes) = 0x%08x, the same as the unseeded crc32c; want the seeds to differ", got)
	}
}

// TestSeededCodecAllocatesNothing pins the seeded path at zero allocations
// per operation. The identity reaches the checksum as a seed the codec
// derives without ever slicing the identity: a slice handed to hash/crc32
// moves the array to the heap, one allocation on every seeded encode and
// decode, so the test drives the codec with a fresh local identity — a
// local is what escapes, while a package-level one is already on the heap
// and would hide it. The race detector changes allocation counts too much
// to compare.
func TestSeededCodecAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}

	b := readGolden(t, "seal")[:28]
	identity := testIdentity // a local: what a call site passes, not a package value

	if got := testing.AllocsPerRun(1000, func() { sinkCRC = crc32cSeeded(identitySeed(identity), b) }); got != 0 {
		t.Errorf("crc32cSeeded allocates %v times per call, want 0", got)
	}

	// Enough capacity that the append itself needs none: what remains is
	// what the checksum helper alone costs the write path.
	buf := make([]byte, 0, SealMarkerSize)
	if got := testing.AllocsPerRun(1000, func() {
		clear(buf)
		buf = AppendSealMarker(buf[:0], identity, testSealMarker)
	}); got != 0 {
		t.Errorf("AppendSealMarker allocates %v times per call, want 0", got)
	}

	// The read path pays the same helper, so the decode is pinned too.
	var m SealMarker
	if got := testing.AllocsPerRun(1000, func() { m, sinkErr = DecodeSealMarker(buf, identity, testSegment, testOffset) }); got != 0 {
		t.Errorf("DecodeSealMarker allocates %v times per call, want 0", got)
	}
	if m != testSealMarker || sinkErr != nil {
		t.Fatalf("DecodeSealMarker = %+v, %v, want the test marker and no error", m, sinkErr)
	}
}

// sinkCRC and sinkErr keep the seeded codec's results alive across
// AllocsPerRun iterations; sinkErr is reset by every clean decode above.
var (
	sinkCRC uint32
	sinkErr error
)
