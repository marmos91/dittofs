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
