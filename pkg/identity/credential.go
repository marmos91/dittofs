package identity

import (
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// DefaultBcryptCost is the default cost parameter for bcrypt hashing.
// Cost 10 provides a good balance between security and performance.
const DefaultBcryptCost = 10

// ErrInvalidCredentials is returned when credentials are invalid.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrPasswordTooShort is returned when a password is too short.
var ErrPasswordTooShort = errors.New("password must be at least 8 characters")

// ErrPasswordTooLong is returned when a password is too long.
// bcrypt has a maximum input length of 72 bytes.
var ErrPasswordTooLong = errors.New("password must be at most 72 characters")

// MinPasswordLength is the minimum required password length.
const MinPasswordLength = 8

// MaxPasswordLength is the maximum allowed password length.
// bcrypt silently truncates at 72 bytes, so we enforce this limit.
const MaxPasswordLength = 72

// HashPassword creates a bcrypt hash of the given password.
//
// Parameters:
//   - password: The plaintext password to hash
//
// Returns:
//   - string: The bcrypt hash
//   - error: If the password is invalid or hashing fails
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
//
// Higher cost values increase security but also increase hashing time.
// Valid cost values are between 4 and 31.
//
// Parameters:
//   - password: The plaintext password to hash
//   - cost: The bcrypt cost parameter (4-31)
//
// Returns:
//   - string: The bcrypt hash
//   - error: If the password is invalid or hashing fails
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
//
// Parameters:
//   - password: The plaintext password to hash
//
// Returns:
//   - passwordHash: The bcrypt hash
//   - ntHashHex: The NT hash as a hex-encoded string
//   - error: If the password is invalid or hashing fails
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
//
// Parameters:
//   - password: The plaintext password to verify
//   - hash: The bcrypt hash to compare against
//
// Returns:
//   - bool: true if the password matches, false otherwise
func VerifyPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// ValidatePassword checks if a password meets the requirements.
//
// Requirements:
//   - At least 8 characters
//   - At most 72 characters (bcrypt limit)
//
// Returns:
//   - error: nil if valid, otherwise an error describing the issue
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
//
// This can happen when:
//   - The cost parameter has been increased
//   - The hash algorithm has been updated
//
// Parameters:
//   - hash: The existing bcrypt hash
//
// Returns:
//   - bool: true if the hash should be regenerated
func NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return true
	}
	return cost < DefaultBcryptCost
}
