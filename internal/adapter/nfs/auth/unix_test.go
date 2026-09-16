package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth"
)

// encodeUnixAuth builds XDR-encoded AUTH_UNIX credentials (RFC 1831 9.2).
func encodeUnixAuth(uid, gid uint32, gids []uint32) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint32(12345))
	machine := "testhost"
	_ = binary.Write(buf, binary.BigEndian, uint32(len(machine)))
	buf.WriteString(machine)
	for i := uint32(0); i < (4-(uint32(len(machine))%4))%4; i++ {
		buf.WriteByte(0)
	}
	_ = binary.Write(buf, binary.BigEndian, uid)
	_ = binary.Write(buf, binary.BigEndian, gid)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(gids)))
	for _, g := range gids {
		_ = binary.Write(buf, binary.BigEndian, g)
	}
	return buf.Bytes()
}

func TestUnixTranslator_Method(t *testing.T) {
	if got := NewUnixTranslator().Method(); got != "unix" {
		t.Fatalf("Method() = %q, want unix", got)
	}
}

// The wire credentials are authoritative and must survive translation
// unchanged: AUTH_UNIX asserts an identity, it does not prove one.
func TestUnixTranslator_CarriesWireCredentials(t *testing.T) {
	res, challenge, err := NewUnixTranslator().Translate(context.Background(), encodeUnixAuth(1000, 2000, []uint32{2000, 3000}))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if challenge != nil {
		t.Fatalf("AUTH_UNIX must not return a challenge, got %v", challenge)
	}
	if *res.Identity.UID != 1000 || *res.Identity.GID != 2000 {
		t.Fatalf("uid/gid = %d/%d, want 1000/2000", *res.Identity.UID, *res.Identity.GID)
	}
	if len(res.Identity.GIDs) != 2 || res.Identity.GIDs[0] != 2000 || res.Identity.GIDs[1] != 3000 {
		t.Fatalf("gids = %v, want [2000 3000]", res.Identity.GIDs)
	}
	if res.Identity.Username != "" {
		t.Fatalf("username = %q, want empty (resolution happens downstream)", res.Identity.Username)
	}
	if res.Method != "unix" || res.Guest {
		t.Fatalf("method/guest = %q/%v, want unix/false", res.Method, res.Guest)
	}
}

func TestUnixTranslator_Malformed(t *testing.T) {
	for name, token := range map[string][]byte{
		"empty":     {},
		"truncated": {0, 0, 0},
	} {
		if _, _, err := NewUnixTranslator().Translate(context.Background(), token); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

// AUTH_UNIX is single-round by definition, so the sentinel must never escape.
func TestUnixTranslator_NeverNeedsMoreProcessing(t *testing.T) {
	_, _, err := NewUnixTranslator().Translate(context.Background(), encodeUnixAuth(0, 0, nil))
	if errors.Is(err, auth.ErrMoreProcessingRequired) {
		t.Fatal("AUTH_UNIX must never return ErrMoreProcessingRequired")
	}
}
