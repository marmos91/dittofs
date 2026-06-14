package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	xdr "github.com/rasky/go-xdr/xdr2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Helper Functions
// ============================================================================

func validAuthUnixCredentials() *UnixAuth {
	return &UnixAuth{
		Stamp:       uint32(time.Now().Unix()),
		MachineName: "testhost",
		UID:         1000,
		GID:         1000,
		GIDs:        []uint32{4, 24, 27, 30},
	}
}

func encodeAuthUnix(auth *UnixAuth) []byte {
	buf := new(bytes.Buffer)

	_ = binary.Write(buf, binary.BigEndian, auth.Stamp)

	nameLen := uint32(len(auth.MachineName))
	_ = binary.Write(buf, binary.BigEndian, nameLen)
	buf.WriteString(auth.MachineName)
	padding := (4 - (nameLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		buf.WriteByte(0)
	}

	_ = binary.Write(buf, binary.BigEndian, auth.UID)
	_ = binary.Write(buf, binary.BigEndian, auth.GID)

	_ = binary.Write(buf, binary.BigEndian, uint32(len(auth.GIDs)))
	for _, gid := range auth.GIDs {
		_ = binary.Write(buf, binary.BigEndian, gid)
	}

	return buf.Bytes()
}

// ============================================================================
// ParseUnixAuth Tests
// ============================================================================

func TestParseUnixAuth(t *testing.T) {
	t.Run("ParsesValidCredentials", func(t *testing.T) {
		original := validAuthUnixCredentials()
		body := encodeAuthUnix(original)

		parsed, err := ParseUnixAuth(body)
		require.NoError(t, err)
		assert.Equal(t, original.Stamp, parsed.Stamp)
		assert.Equal(t, original.MachineName, parsed.MachineName)
		assert.Equal(t, original.UID, parsed.UID)
		assert.Equal(t, original.GID, parsed.GID)
		assert.Equal(t, original.GIDs, parsed.GIDs)
	})

	t.Run("ParsesRootCredentials", func(t *testing.T) {
		auth := &UnixAuth{
			Stamp:       uint32(time.Now().Unix()),
			MachineName: "testhost",
			UID:         0,
			GID:         0,
			GIDs:        []uint32{},
		}
		body := encodeAuthUnix(auth)

		parsed, err := ParseUnixAuth(body)
		require.NoError(t, err)
		assert.Equal(t, uint32(0), parsed.UID)
		assert.Equal(t, uint32(0), parsed.GID)
		assert.Empty(t, parsed.GIDs)
	})

	t.Run("ParsesWithMaximumGroups", func(t *testing.T) {
		gids := make([]uint32, 16)
		for i := range gids {
			gids[i] = uint32(i + 1000)
		}

		auth := &UnixAuth{
			Stamp:       12345,
			MachineName: "testhost",
			UID:         1000,
			GID:         1000,
			GIDs:        gids,
		}
		body := encodeAuthUnix(auth)

		parsed, err := ParseUnixAuth(body)
		require.NoError(t, err)
		assert.Len(t, parsed.GIDs, 16)
		assert.Equal(t, gids, parsed.GIDs)
	})

	t.Run("RejectsExcessiveGroups", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(12345))
		_ = binary.Write(buf, binary.BigEndian, uint32(8))
		_, _ = buf.WriteString("testhost")
		_ = binary.Write(buf, binary.BigEndian, uint32(1000))
		_ = binary.Write(buf, binary.BigEndian, uint32(1000))
		_ = binary.Write(buf, binary.BigEndian, uint32(17)) // Too many groups

		_, err := ParseUnixAuth(buf.Bytes())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too many gids")
	})

	t.Run("RejectsLongMachineName", func(t *testing.T) {
		buf := new(bytes.Buffer)
		_ = binary.Write(buf, binary.BigEndian, uint32(12345))
		_ = binary.Write(buf, binary.BigEndian, uint32(256)) // Too long

		_, err := ParseUnixAuth(buf.Bytes())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "machine name too long")
	})

	t.Run("RejectsEmptyBody", func(t *testing.T) {
		_, err := ParseUnixAuth([]byte{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("HandlesEmptyMachineName", func(t *testing.T) {
		auth := &UnixAuth{
			Stamp:       12345,
			MachineName: "",
			UID:         1000,
			GID:         1000,
			GIDs:        []uint32{},
		}
		body := encodeAuthUnix(auth)

		parsed, err := ParseUnixAuth(body)
		require.NoError(t, err)
		assert.Equal(t, "", parsed.MachineName)
	})
}

// ============================================================================
// UnixAuthString Tests
// ============================================================================

func TestUnixAuthString(t *testing.T) {
	t.Run("FormatsCorrectly", func(t *testing.T) {
		auth := &UnixAuth{
			Stamp:       12345,
			MachineName: "testhost",
			UID:         1000,
			GID:         1000,
			GIDs:        []uint32{4, 24, 27, 30},
		}

		str := auth.String()
		assert.Contains(t, str, "testhost")
		assert.Contains(t, str, "1000")
		assert.Contains(t, str, "[4 24 27 30]")
	})

	t.Run("FormatsEmptyGroups", func(t *testing.T) {
		auth := &UnixAuth{
			Stamp:       12345,
			MachineName: "testhost",
			UID:         1000,
			GID:         1000,
			GIDs:        []uint32{},
		}

		str := auth.String()
		assert.Contains(t, str, "testhost")
		assert.Contains(t, str, "[]")
	})
}

// ============================================================================
// AuthFlavors Tests
// ============================================================================

func TestAuthFlavors(t *testing.T) {
	t.Run("AuthNullValue", func(t *testing.T) {
		assert.Equal(t, uint32(0), AuthNull)
	})

	t.Run("AuthUnixValue", func(t *testing.T) {
		assert.Equal(t, uint32(1), AuthUnix)
	})

	t.Run("AuthShortValue", func(t *testing.T) {
		assert.Equal(t, uint32(2), AuthShort)
	})

	t.Run("AuthDESValue", func(t *testing.T) {
		assert.Equal(t, uint32(3), AuthDES)
	})

	t.Run("FlavorsAreUnique", func(t *testing.T) {
		flavors := []uint32{AuthNull, AuthUnix, AuthShort, AuthDES}

		seen := make(map[uint32]bool)
		for _, flavor := range flavors {
			assert.False(t, seen[flavor], "flavor %d is not unique", flavor)
			seen[flavor] = true
		}
	})
}

// ============================================================================
// MakeProgMismatchReply Tests
// ============================================================================

func TestMakeProgMismatchReply(t *testing.T) {
	t.Run("GeneratesValidReply", func(t *testing.T) {
		xid := uint32(0x12345678)
		low := uint32(3)
		high := uint32(3)

		reply, err := MakeProgMismatchReply(xid, low, high)
		require.NoError(t, err)
		require.NotNil(t, reply)

		// Verify minimum size: 4 (fragment header) + 24 (RPC reply header) + 8 (mismatch info) = 36
		assert.GreaterOrEqual(t, len(reply), 36)

		// Verify fragment header (first 4 bytes)
		// High bit should be set (last fragment), lower 31 bits = length
		fragHeader := binary.BigEndian.Uint32(reply[0:4])
		assert.True(t, (fragHeader&0x80000000) != 0, "last fragment bit should be set")
		fragLen := fragHeader & 0x7FFFFFFF
		assert.Equal(t, uint32(len(reply)-4), fragLen, "fragment length should match payload")

		// Verify XID is echoed back correctly (bytes 4-7)
		replyXID := binary.BigEndian.Uint32(reply[4:8])
		assert.Equal(t, xid, replyXID, "XID should match")

		// Verify MsgType = REPLY (1) (bytes 8-11)
		msgType := binary.BigEndian.Uint32(reply[8:12])
		assert.Equal(t, uint32(RPCReply), msgType, "MsgType should be REPLY")

		// Verify ReplyState = MSG_ACCEPTED (0) (bytes 12-15)
		replyState := binary.BigEndian.Uint32(reply[12:16])
		assert.Equal(t, uint32(RPCMsgAccepted), replyState, "ReplyState should be MSG_ACCEPTED")
	})

	t.Run("EncodesVersionRange", func(t *testing.T) {
		xid := uint32(0xABCD1234)
		low := uint32(2)
		high := uint32(4)

		reply, err := MakeProgMismatchReply(xid, low, high)
		require.NoError(t, err)

		// The low and high versions are at the end of the reply
		// After fragment header (4) + reply header (~28) = mismatch info at end
		replyLen := len(reply)
		lowVersion := binary.BigEndian.Uint32(reply[replyLen-8 : replyLen-4])
		highVersion := binary.BigEndian.Uint32(reply[replyLen-4 : replyLen])

		assert.Equal(t, low, lowVersion, "low version should be encoded correctly")
		assert.Equal(t, high, highVersion, "high version should be encoded correctly")
	})

	t.Run("HandlesSameVersionForLowAndHigh", func(t *testing.T) {
		// Common case: server only supports one version (NFSv3)
		xid := uint32(0x11111111)
		version := uint32(3)

		reply, err := MakeProgMismatchReply(xid, version, version)
		require.NoError(t, err)
		require.NotNil(t, reply)

		// Verify versions at end
		replyLen := len(reply)
		lowVersion := binary.BigEndian.Uint32(reply[replyLen-8 : replyLen-4])
		highVersion := binary.BigEndian.Uint32(reply[replyLen-4 : replyLen])

		assert.Equal(t, version, lowVersion)
		assert.Equal(t, version, highVersion)
	})

	t.Run("RejectsInvalidVersionRange", func(t *testing.T) {
		// low > high is invalid per RFC 5531
		xid := uint32(0x12345678)
		low := uint32(5)
		high := uint32(3)

		reply, err := MakeProgMismatchReply(xid, low, high)
		require.Error(t, err)
		assert.Nil(t, reply)
		assert.Contains(t, err.Error(), "invalid version range")
		assert.Contains(t, err.Error(), "low (5) > high (3)")
	})

	t.Run("HandlesZeroXID", func(t *testing.T) {
		reply, err := MakeProgMismatchReply(0, 3, 3)
		require.NoError(t, err)
		require.NotNil(t, reply)

		// Verify XID is 0
		replyXID := binary.BigEndian.Uint32(reply[4:8])
		assert.Equal(t, uint32(0), replyXID)
	})

	t.Run("HandlesMaxXID", func(t *testing.T) {
		maxXID := uint32(0xFFFFFFFF)
		reply, err := MakeProgMismatchReply(maxXID, 3, 3)
		require.NoError(t, err)
		require.NotNil(t, reply)

		// Verify XID is max value
		replyXID := binary.BigEndian.Uint32(reply[4:8])
		assert.Equal(t, maxXID, replyXID)
	})

	t.Run("ContainsProgMismatchStatus", func(t *testing.T) {
		reply, err := MakeProgMismatchReply(0x1234, 3, 3)
		require.NoError(t, err)

		// AcceptStat should be PROG_MISMATCH (2)
		// Position: after fragment header (4) + XID (4) + MsgType (4) + ReplyState (4) + Verifier (8) = 24
		// So AcceptStat is at bytes 24-27
		acceptStat := binary.BigEndian.Uint32(reply[24:28])
		assert.Equal(t, uint32(RPCProgMismatch), acceptStat, "AcceptStat should be PROG_MISMATCH")
	})
}

// marshalCall encodes a complete RPC call message (XID..Verf) plus procedure
// data, the way a client would put it on the wire.
func marshalCall(t *testing.T, call *RPCCallMessage, procData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := xdr.Marshal(&buf, call); err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	buf.Write(procData)
	return buf.Bytes()
}

func TestHeaderMICPreimage(t *testing.T) {
	// The MIC preimage must be XID..end-of-credential, NOT including the
	// verifier or the procedure data (RFC 2203 Section 5.3.3.2).
	t.Run("excludes verifier and body", func(t *testing.T) {
		call := &RPCCallMessage{
			XID:        0xdeadbeef,
			MsgType:    RPCCall,
			RPCVersion: 2,
			Program:    100003,
			Version:    3,
			Procedure:  1,
			Cred:       OpaqueAuth{Flavor: AuthRPCSECGSS, Body: []byte("the-gss-credential!")}, // 19 bytes -> 1 pad
			Verf:       OpaqueAuth{Flavor: AuthRPCSECGSS, Body: []byte("the-mic-token")},
		}
		message := marshalCall(t, call, []byte("procedure-arguments"))

		preimage, err := HeaderMICPreimage(message)
		require.NoError(t, err)

		// Expected end: 24 (fixed header) + 4 (cred flavor) + 4 (cred len) +
		// 19 (cred body) + 1 (XDR pad) = 52.
		require.Equal(t, 52, len(preimage))
		// Preimage must be a prefix of the message.
		require.Equal(t, message[:52], preimage)
		// Must contain the credential body...
		require.Contains(t, string(preimage), "the-gss-credential!")
		// ...but NOT the verifier MIC or the procedure args.
		require.NotContains(t, string(preimage), "the-mic-token")
		require.NotContains(t, string(preimage), "procedure-arguments")
	})

	t.Run("matches ReadData boundary", func(t *testing.T) {
		// The preimage end must equal where ReadData starts the procedure body
		// minus the verifier, i.e. preimage covers exactly through the cred.
		call := &RPCCallMessage{
			XID:        1,
			MsgType:    RPCCall,
			RPCVersion: 2,
			Program:    100003,
			Version:    3,
			Procedure:  8,
			Cred:       OpaqueAuth{Flavor: AuthUnix, Body: []byte{1, 2, 3, 4}}, // aligned
			Verf:       OpaqueAuth{Flavor: AuthNull, Body: nil},
		}
		message := marshalCall(t, call, []byte("WRITE-args"))

		preimage, err := HeaderMICPreimage(message)
		require.NoError(t, err)
		// 24 + 4 + 4 + 4 = 36.
		require.Equal(t, 36, len(preimage))
		require.Equal(t, message[:36], preimage)
	})

	t.Run("rejects truncated message", func(t *testing.T) {
		_, err := HeaderMICPreimage([]byte{0, 0, 0, 1})
		require.Error(t, err)
	})

	t.Run("rejects overrunning credential length", func(t *testing.T) {
		// Fixed header (24) + cred flavor (4) + a bogus huge cred length.
		msg := make([]byte, 32)
		binary.BigEndian.PutUint32(msg[28:32], 0xffffffff) // cred len overruns
		_, err := HeaderMICPreimage(msg)
		require.Error(t, err)
	})
}

// ============================================================================
// MakeSuccessReply Tests
// ============================================================================

func TestMakeSuccessReply(t *testing.T) {
	t.Run("WireFormatAndRecordMarker", func(t *testing.T) {
		xid := uint32(0xABCD1234)
		data := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}

		reply, err := MakeSuccessReply(xid, data)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(reply), 4)

		// Fragment header: last-fragment bit set, length = everything after it.
		fragHeader := binary.BigEndian.Uint32(reply[0:4])
		assert.NotZero(t, fragHeader&0x80000000, "last fragment bit must be set")
		assert.Equal(t, uint32(len(reply)-4), fragHeader&0x7FFFFFFF,
			"fragment length must equal record body length")

		// The XDR-decoded reply body must round-trip the original fields, and
		// the trailing bytes must equal the supplied procedure data verbatim.
		var decoded RPCReplyMessage
		n, err := xdr.Unmarshal(bytes.NewReader(reply[4:]), &decoded)
		require.NoError(t, err)
		assert.Equal(t, xid, decoded.XID)
		assert.Equal(t, uint32(RPCReply), decoded.MsgType)
		assert.Equal(t, uint32(RPCMsgAccepted), decoded.ReplyState)
		assert.Equal(t, uint32(RPCSuccess), decoded.AcceptStat)
		assert.Equal(t, data, reply[4+n:], "procedure data must be appended verbatim")
	})

	t.Run("HandlesEmptyData", func(t *testing.T) {
		reply, err := MakeSuccessReply(1, nil)
		require.NoError(t, err)
		fragHeader := binary.BigEndian.Uint32(reply[0:4])
		assert.NotZero(t, fragHeader&0x80000000)
		assert.Equal(t, uint32(len(reply)-4), fragHeader&0x7FFFFFFF)
	})
}

func TestParseUnixAuth_Truncated(t *testing.T) {
	full := encodeAuthUnix(validAuthUnixCredentials())

	// Every strict prefix shorter than the full body is malformed and must
	// produce an error rather than a partial/garbage credential.
	for n := 1; n < len(full); n++ {
		_, err := ParseUnixAuth(full[:n])
		// A prefix is only valid if it happens to land exactly on the end of
		// the GID array; otherwise it must error. The full body's final field
		// is the last GID, so any strict prefix is short.
		require.Error(t, err, "prefix length %d should be rejected", n)
	}
}
