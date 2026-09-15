package nfs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
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

// TestDemuxBackchannelReply_ConsumesAReplyWithNoTable pins what happens to a
// callback reply that outlives the session it belonged to. Teardown removes the
// pending-reply table, but the TCP connection stays up and carries other
// sessions — so a reply still in flight arrives with nothing to route it to.
//
// Handing it back as "not a reply" sends a msg_type=REPLY into rpc.ReadCall,
// which rejects it as a non-CALL and closes the socket. One late reply to a
// retired session would take every other session on that connection with it.
func TestDemuxBackchannelReply_ConsumesAReplyWithNoTable(t *testing.T) {
	reply := make([]byte, 8)
	binary.BigEndian.PutUint32(reply[0:4], 0xDEADBEEF)
	binary.BigEndian.PutUint32(reply[4:8], rpc.RPCReply)

	if !DemuxBackchannelReply(reply, 42, func() *state.PendingCBReplies { return nil }) {
		t.Error("a REPLY with no pending-reply table was handed back as a CALL: " +
			"rpc.ReadCall rejects it and closes a connection other sessions are using")
	}
}

// A CALL is still a CALL with no table — the fore channel must not be consumed.
func TestDemuxBackchannelReply_LeavesACallAlone(t *testing.T) {
	call := make([]byte, 8)
	binary.BigEndian.PutUint32(call[0:4], 0x01020304)
	binary.BigEndian.PutUint32(call[4:8], rpc.RPCCall)

	if DemuxBackchannelReply(call, 42, func() *state.PendingCBReplies { return nil }) {
		t.Error("a CALL was consumed as a backchannel reply")
	}
}
