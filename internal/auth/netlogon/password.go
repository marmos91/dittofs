package netlogon

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"unicode/utf16"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
)

// machinePasswordLen is the length of a generated machine-account password.
// AD machine accounts use a 120-character random password by default; we use a
// comfortably-long 64 to stay well under the 127-UTF16-char NL_TRUST_PASSWORD
// limit while remaining brute-force-infeasible.
const machinePasswordLen = 64

// machinePasswordAlphabet is the character set for generated machine passwords.
// It deliberately excludes characters that complicate shell/config round-trips
// while keeping >90 bits of entropy at the configured length.
const machinePasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.@#%+=:"

// trustPasswordBufferRunes is the fixed NL_TRUST_PASSWORD buffer size in UTF-16
// code units (512 bytes). MS-NRPC §2.2.1.3.7: the password occupies the END of
// the buffer; the leading bytes are random filler.
const trustPasswordBufferRunes = 256

// generateMachinePassword returns a cryptographically random machine-account
// password of machinePasswordLen characters drawn from machinePasswordAlphabet.
func generateMachinePassword() (string, error) {
	out := make([]byte, machinePasswordLen)
	max := big.NewInt(int64(len(machinePasswordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("netlogon: generate machine password: %w", err)
		}
		out[i] = machinePasswordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// encryptor is the subset of the go-msrpc secure-channel client used to build a
// NetrServerPasswordSet2 request: it encrypts bytes with the negotiated session
// key (AES-CFB for AES_SHA2 channels, else DES) and sequences the per-call
// NETLOGON authenticators. *xxx_SecureChannelClient satisfies it via its
// exported Encrypt/SetAuthenticators/VerifyAuthenticator methods (which are not
// part of the LogonSecureChannelClient interface, so we type-assert to this).
type encryptor interface {
	Encrypt(ctx context.Context, b []byte) ([]byte, error)
	SetAuthenticators(ctx context.Context, a, ra **logon.Authenticator) error
	VerifyAuthenticator(ctx context.Context, ra *logon.Authenticator) error
}

// buildTrustPassword constructs an NL_TRUST_PASSWORD for the given cleartext
// password, then encrypts it with the secure channel's session key as
// NetrServerPasswordSet2 requires (MS-NRPC §3.4.5.2.6).
//
// The cleartext buffer layout (NL_TRUST_PASSWORD, §2.2.1.3.7):
//   - Buffer: 256 UTF-16 code units (512 bytes). The password's UTF-16LE bytes
//     occupy the tail; the leading bytes are random filler.
//   - Length: the password length in BYTES (UTF-16LE), placed in the trailing
//     uint32 of the encrypted structure.
//
// The whole 516-byte structure (512-byte buffer + 4-byte length) is encrypted
// with the session key. go-msrpc marshals Buffer ([]uint16) then Length
// (uint32), and the secure channel's Encrypt covers exactly those bytes, so we
// encrypt the marshalled bytes ourselves and hand the cipher back as the
// TrustPassword.
func buildTrustPassword(ctx context.Context, enc encryptor, password string) (*logon.TrustPassword, error) {
	codes := utf16.Encode([]rune(password))
	pwBytes := len(codes) * 2
	if len(codes) > trustPasswordBufferRunes {
		return nil, fmt.Errorf("netlogon: machine password too long (%d > %d UTF-16 chars)", len(codes), trustPasswordBufferRunes)
	}

	// 512-byte cleartext buffer: random filler at the front, password UTF-16LE
	// at the tail.
	clear := make([]byte, trustPasswordBufferRunes*2+4)
	if _, err := rand.Read(clear[:trustPasswordBufferRunes*2]); err != nil {
		return nil, fmt.Errorf("netlogon: trust password filler: %w", err)
	}
	off := trustPasswordBufferRunes*2 - pwBytes
	for i, c := range codes {
		clear[off+i*2] = byte(c)
		clear[off+i*2+1] = byte(c >> 8)
	}
	// Trailing length (bytes) little-endian.
	binary.LittleEndian.PutUint32(clear[trustPasswordBufferRunes*2:], uint32(pwBytes))

	cipher, err := enc.Encrypt(ctx, clear)
	if err != nil {
		return nil, fmt.Errorf("netlogon: encrypt trust password: %w", err)
	}
	if len(cipher) != len(clear) {
		// The secure channel's Encrypt is length-preserving only for the AES-CFB
		// path (CapAES_SHA2). On a legacy DES/RC4-negotiated channel go-msrpc's
		// Encrypt returns a truncated block, which cannot carry the 512-byte
		// password buffer — rotation is unsupported there. Modern AD/Samba always
		// negotiate AES_SHA2, so fail fast with an actionable message rather than
		// sending a malformed NL_TRUST_PASSWORD.
		return nil, fmt.Errorf("netlogon: trust-password encryption is not length-preserving (%d != %d); the secure channel did not negotiate AES (CapAES_SHA2), so NetrServerPasswordSet2 rotation is unsupported on this channel", len(cipher), len(clear))
	}

	// Re-split the ciphertext into the Buffer ([]uint16) + Length (uint32) the
	// marshaller expects.
	buf := make([]uint16, trustPasswordBufferRunes)
	for i := 0; i < trustPasswordBufferRunes; i++ {
		buf[i] = uint16(cipher[i*2]) | uint16(cipher[i*2+1])<<8
	}
	encLen := binary.LittleEndian.Uint32(cipher[trustPasswordBufferRunes*2:])

	return &logon.TrustPassword{Buffer: buf, Length: encLen}, nil
}
