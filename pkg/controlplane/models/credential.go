package models

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"unicode/utf16"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/md4" //nolint:staticcheck // MD4 is required for NTLM protocol compatibility
)

// DefaultBcryptCost is the default cost parameter for bcrypt hashing.
// Cost 10 provides a good balance between security and performance.
const DefaultBcryptCost = 10

// Password validation errors.
var (
	// ErrInvalidCredentials is returned when credentials are invalid.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrPasswordTooShort is returned when a password is too short.
	ErrPasswordTooShort = errors.New("password must be at least 8 characters")

	// ErrPasswordTooLong is returned when a password is too long.
	// bcrypt has a maximum input length of 72 bytes.
	ErrPasswordTooLong = errors.New("password must be at most 72 characters")
)

// Password length constraints.
const (
	// MinPasswordLength is the minimum required password length.
	MinPasswordLength = 8

	// MaxPasswordLength is the maximum allowed password length.
	// bcrypt silently truncates at 72 bytes, so we enforce this limit.
	MaxPasswordLength = 72
)

// HashPassword creates a bcrypt hash of the given password.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), DefaultBcryptCost)
	if err != nil {
		return "", err
	}

	return string(hash), nil
}

// HashPasswordWithCost creates a bcrypt hash with a custom cost.
// Higher cost values increase security but also increase hashing time.
// Valid cost values are between 4 and 31.
func HashPasswordWithCost(password string, cost int) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", err
	}

	return string(hash), nil
}

// HashPasswordWithNT creates both a bcrypt hash and NT hash for a password.
// This is a convenience function for user creation/password update flows
// that need both hashes computed together.
func HashPasswordWithNT(password string) (passwordHash, ntHashHex string, err error) {
	passwordHash, err = HashPassword(password)
	if err != nil {
		return "", "", err
	}

	ntHash := ComputeNTHash(password)
	ntHashHex = hex.EncodeToString(ntHash[:])

	return passwordHash, ntHashHex, nil
}

// VerifyPassword checks if a password matches a bcrypt hash.
func VerifyPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// ValidatePassword checks if a password meets the requirements.
// Requirements: at least 8 characters, at most 72 characters (bcrypt limit).
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// NeedsRehash checks if a hash needs to be regenerated.
// This can happen when the cost parameter has been increased
// or the hash algorithm has been updated.
func NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return true
	}
	return cost < DefaultBcryptCost
}

// ComputeNTHash computes the NT hash from a password.
// The NT hash is: MD4(UTF16LE(password))
// This is required for SMB NTLM authentication.
func ComputeNTHash(password string) [16]byte {
	// Convert password to UTF-16LE
	utf16Password := utf16.Encode([]rune(password))
	passwordBytes := make([]byte, len(utf16Password)*2)
	for i, r := range utf16Password {
		binary.LittleEndian.PutUint16(passwordBytes[i*2:], r)
	}

	// Compute MD4 hash
	h := md4.New()
	h.Write(passwordBytes)
	var ntHash [16]byte
	copy(ntHash[:], h.Sum(nil))
	return ntHash
}
