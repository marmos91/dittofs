package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

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
