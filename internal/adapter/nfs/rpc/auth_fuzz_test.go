package rpc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	xdr "github.com/rasky/go-xdr/xdr2"
	"github.com/stretchr/testify/require"
)

func FuzzParseUnixAuth(f *testing.F) {
	groups := []uint32{0, 1, 0x7fffffff, 0x80000000, 0xffffffff, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	for _, nameLen := range []int{0, 1, 2, 3, 4, 255, 256} {
		body := encodeAuthUnix(&UnixAuth{
			Stamp:       0xffffffff,
			MachineName: strings.Repeat("m", nameLen),
			UID:         0x80000000,
			GID:         0xffffffff,
			GIDs:        groups,
		})
		f.Add(body)

		// The parser ignores padding contents and refuses trailing bytes.
		// Exercise both across all string alignments.
		padded := bytes.Clone(body)
		paddingEnd := 8 + nameLen + (4-nameLen%4)%4
		for i := 8 + nameLen; i < paddingEnd; i++ {
			padded[i] = 0xff
		}
		f.Add(padded)
		f.Add(append(bytes.Clone(body), 0xff, 0x00, 0x80))

		if nameLen == 255 {
			// Cover truncation inside every field, including string padding
			// and each of the sixteen supplementary groups.
			for n := 0; n < len(body); n++ {
				f.Add(body[:n])
			}
		}
	}
	f.Add(encodeAuthUnix(&UnixAuth{}))
	f.Add(encodeAuthUnix(&UnixAuth{MachineName: "\x00\xff\x80", GIDs: []uint32{0xffffffff}}))
	f.Add(encodeAuthUnix(&UnixAuth{GIDs: make([]uint32, 17)}))

	// Advertised lengths must be rejected before they can drive an
	// allocation, even when their contents are absent.
	longName := make([]byte, 8)
	binary.BigEndian.PutUint32(longName[4:], 0xffffffff)
	f.Add(longName)
	tooManyGroups := encodeAuthUnix(&UnixAuth{})
	binary.BigEndian.PutUint32(tooManyGroups[len(tooManyGroups)-4:], 0xffffffff)
	f.Add(tooManyGroups)

	f.Fuzz(func(t *testing.T, body []byte) {
		// The defined credential fields consume at most 340 bytes. Leave room
		// for refused suffixes while bounding each fuzz iteration's work.
		if len(body) > 4096 {
			t.Skip()
		}

		original := bytes.Clone(body)
		got, err := ParseUnixAuth(body)
		require.True(t, bytes.Equal(original, body), "parsing must not modify the credential body")

		// Use the independent XDR decoder as the wire-format oracle. Its
		// limit bounds both string bytes and array elements before allocation;
		// the AUTH_UNIX supplementary-group limit is stricter than that. A body
		// is exactly one credential, so bytes the oracle leaves unread are
		// malformed too.
		want := UnixAuth{GIDs: []uint32{}}
		n, decodeErr := xdr.UnmarshalLimited(bytes.NewReader(body), &want, 255)
		if decodeErr != nil || n != len(body) || len(want.GIDs) > 16 {
			require.Error(t, err, "malformed or oversized credentials must be rejected")
			require.Nil(t, got, "failed parsing must not return a partial identity")
			return
		}

		require.NoError(t, err)
		require.Equal(t, &want, got, "all wire identity fields must survive decoding unchanged")
	})
}
