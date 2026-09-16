package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
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

type stubStore struct {
	models.IdentityStore
	user *models.User
	err  error
}

func (s stubStore) GetUserByUID(context.Context, uint32) (*models.User, error) {
	return s.user, s.err
}

func TestUnixTranslator_Method(t *testing.T) {
	if got := NewUnixTranslator(nil).Method(); got != "unix" {
		t.Fatalf("Method() = %q, want unix", got)
	}
}

// The wire credentials are authoritative and must survive translation
// unchanged: AUTH_UNIX asserts an identity, it does not prove one.
func TestUnixTranslator_CarriesWireCredentials(t *testing.T) {
	res, challenge, err := NewUnixTranslator(nil).Translate(context.Background(), encodeUnixAuth(1000, 2000, []uint32{2000, 3000}))
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
	if res.Identity.Username != "unix:1000" {
		t.Fatalf("unknown uid username = %q, want unix:1000", res.Identity.Username)
	}
	if res.Method != "unix" || res.Guest {
		t.Fatalf("method/guest = %q/%v, want unix/false", res.Method, res.Guest)
	}
}

func TestUnixTranslator_KnownUIDUsesUsername(t *testing.T) {
	store := stubStore{user: &models.User{Username: "alice"}}
	res, _, err := NewUnixTranslator(store).Translate(context.Background(), encodeUnixAuth(1000, 1000, nil))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Identity.Username != "alice" {
		t.Fatalf("username = %q, want alice", res.Identity.Username)
	}
}

// A store error must not fail authentication: the wire credentials still
// identify the caller, so translation falls back to the synthetic name.
func TestUnixTranslator_StoreErrorFallsBackToSynthetic(t *testing.T) {
	store := stubStore{err: errors.New("store down")}
	res, _, err := NewUnixTranslator(store).Translate(context.Background(), encodeUnixAuth(1000, 1000, nil))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Identity.Username != "unix:1000" {
		t.Fatalf("username = %q, want unix:1000", res.Identity.Username)
	}
}

func TestUnixTranslator_Malformed(t *testing.T) {
	for name, token := range map[string][]byte{
		"empty":     {},
		"truncated": {0, 0, 0},
	} {
		if _, _, err := NewUnixTranslator(nil).Translate(context.Background(), token); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

// AUTH_UNIX is single-round by definition, so the sentinel must never escape.
func TestUnixTranslator_NeverNeedsMoreProcessing(t *testing.T) {
	_, _, err := NewUnixTranslator(nil).Translate(context.Background(), encodeUnixAuth(0, 0, nil))
	if errors.Is(err, auth.ErrMoreProcessingRequired) {
		t.Fatal("AUTH_UNIX must never return ErrMoreProcessingRequired")
	}
}
