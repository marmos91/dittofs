package netlogon

import (
	"context"
	"testing"
	"unicode/utf16"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
)

func TestGenerateMachinePassword(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		p, err := generateMachinePassword()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(p) != machinePasswordLen {
			t.Fatalf("length = %d, want %d", len(p), machinePasswordLen)
		}
		for _, c := range p {
			if !containsRune(machinePasswordAlphabet, c) {
				t.Fatalf("password contains out-of-alphabet rune %q", c)
			}
		}
		if _, dup := seen[p]; dup {
			t.Fatalf("generated a duplicate password in %d tries (not random)", i)
		}
		seen[p] = struct{}{}
	}
}

// identityEncryptor is a no-op encryptor: Encrypt returns the cleartext
// unchanged so the test can decode the NL_TRUST_PASSWORD buffer back to the
// original password and verify the byte layout.
type identityEncryptor struct{}

func (identityEncryptor) Encrypt(_ context.Context, b []byte) ([]byte, error) {
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}
func (identityEncryptor) SetAuthenticators(context.Context, **logon.Authenticator, **logon.Authenticator) error {
	return nil
}
func (identityEncryptor) VerifyAuthenticator(context.Context, *logon.Authenticator) error { return nil }

func TestBuildTrustPassword_Layout(t *testing.T) {
	const pw = "Hello-World_123"
	tp, err := buildTrustPassword(context.Background(), identityEncryptor{}, pw)
	if err != nil {
		t.Fatalf("buildTrustPassword: %v", err)
	}

	wantLen := uint32(len([]rune(pw)) * 2)
	if tp.Length != wantLen {
		t.Fatalf("Length = %d, want %d (UTF-16 bytes)", tp.Length, wantLen)
	}
	if len(tp.Buffer) != trustPasswordBufferRunes {
		t.Fatalf("Buffer len = %d, want %d", len(tp.Buffer), trustPasswordBufferRunes)
	}

	// The password sits at the TAIL of the buffer; decode it back.
	pwRunes := utf16.Encode([]rune(pw))
	off := trustPasswordBufferRunes - len(pwRunes)
	for i, want := range pwRunes {
		if tp.Buffer[off+i] != want {
			t.Fatalf("buffer[%d] = %04x, want %04x", off+i, tp.Buffer[off+i], want)
		}
	}
	decoded := string(utf16.Decode(tp.Buffer[off:]))
	if decoded != pw {
		t.Fatalf("decoded tail = %q, want %q", decoded, pw)
	}
}

func TestBuildTrustPassword_TooLong(t *testing.T) {
	long := make([]rune, trustPasswordBufferRunes+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := buildTrustPassword(context.Background(), identityEncryptor{}, string(long)); err == nil {
		t.Fatal("expected error for over-long password")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
