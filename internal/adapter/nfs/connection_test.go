package nfs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frame builds a record-marking fragment: 4-byte header (last flag + length)
// followed by payload.
func frame(last bool, payload []byte) []byte {
	var hdr [4]byte
	v := uint32(len(payload)) & 0x7FFFFFFF
	if last {
		v |= 0x80000000
	}
	binary.BigEndian.PutUint32(hdr[:], v)
	return append(hdr[:], payload...)
}

func TestReadRPCRecord_SingleFragment(t *testing.T) {
	payload := []byte("hello world")
	stream := bytes.NewReader(frame(true, payload))

	hdr, err := ReadFragmentHeader(stream)
	require.NoError(t, err)
	require.True(t, hdr.IsLast)

	msg, err := ReadRPCRecord(stream, hdr, "test")
	require.NoError(t, err)
	assert.Equal(t, payload, msg)
}

func TestReadRPCRecord_MultiFragmentReassembly(t *testing.T) {
	// Three fragments, only the last marked final.
	var stream bytes.Buffer
	stream.Write(frame(false, []byte("part-one-")))
	stream.Write(frame(false, []byte("part-two-")))
	stream.Write(frame(true, []byte("part-three")))

	r := bytes.NewReader(stream.Bytes())
	hdr, err := ReadFragmentHeader(r)
	require.NoError(t, err)
	require.False(t, hdr.IsLast)

	msg, err := ReadRPCRecord(r, hdr, "test")
	require.NoError(t, err)
	assert.Equal(t, "part-one-part-two-part-three", string(msg))
}

func TestReadRPCRecord_EmptyContinuationFragment(t *testing.T) {
	// A zero-length non-final fragment followed by a final fragment.
	var stream bytes.Buffer
	stream.Write(frame(false, []byte("data")))
	stream.Write(frame(false, nil))
	stream.Write(frame(true, []byte("end")))

	r := bytes.NewReader(stream.Bytes())
	hdr, err := ReadFragmentHeader(r)
	require.NoError(t, err)

	msg, err := ReadRPCRecord(r, hdr, "test")
	require.NoError(t, err)
	assert.Equal(t, "dataend", string(msg))
}

func TestReadRPCRecord_ExceedsMaxAfterReassembly(t *testing.T) {
	// First fragment near the cap, a continuation that pushes the cumulative
	// record over MaxFragmentSize must be rejected.
	first := make([]byte, MaxFragmentSize-10)
	var stream bytes.Buffer
	stream.Write(frame(false, first))
	stream.Write(frame(true, make([]byte, 100)))

	r := bytes.NewReader(stream.Bytes())
	hdr, err := ReadFragmentHeader(r)
	require.NoError(t, err)

	_, err = ReadRPCRecord(r, hdr, "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestReadRPCRecord_TruncatedContinuation(t *testing.T) {
	// Non-final first fragment, then EOF before the continuation header.
	r := bytes.NewReader(frame(false, []byte("partial")))
	hdr, err := ReadFragmentHeader(r)
	require.NoError(t, err)

	_, err = ReadRPCRecord(r, hdr, "test")
	require.Error(t, err)
}
