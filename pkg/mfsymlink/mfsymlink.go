// Package mfsymlink implements Minshall+French symlinks (MFsymlinks) encoding and decoding.
//
// MFsymlinks are a symlink format used by macOS and Windows SMB clients.
// Instead of using reparse points, the symlink target is stored as a regular file
// with a special format.
//
// Format (1067 bytes total):
//
//	Offset 0-4:    "XSym\n"              (magic marker)
//	Offset 5-9:    "LLLL\n"              (4-digit target length, zero-padded)
//	Offset 10-42:  "<32-char MD5>\n"     (MD5 hash of target in hex)
//	Offset 43+:    "<target>\n<padding>" (target path + newline + spaces to 1067)
//
// References:
//   - https://wiki.samba.org/index.php/UNIX_Extensions
//   - https://pkg.go.dev/github.com/dhui/mfsymlink
package mfsymlink

import (
	"bytes"
	"crypto/md5" //nolint:gosec // MD5 is required by MFsymlink spec, not used for security
	"encoding/hex"
	"errors"
	"fmt"
)

// Size is the fixed size of an MFsymlink file in bytes.
const Size = 1067

// Magic is the magic marker that identifies an MFsymlink file.
const Magic = "XSym\n"

// MaxTargetLength is the maximum length of a symlink target that can be encoded.
// This is Size - len(Magic) - len("LLLL\n") - len("<md5>\n") - len("\n") = 1067 - 5 - 5 - 33 - 1 = 1023
const MaxTargetLength = 1023

var (
	// ErrInvalidMFsymlink is returned when the data is not a valid MFsymlink.
	ErrInvalidMFsymlink = errors.New("invalid MFsymlink format")

	// ErrTargetTooLong is returned when the symlink target exceeds MaxTargetLength.
	ErrTargetTooLong = errors.New("symlink target too long for MFsymlink format")

	// ErrMD5Mismatch is returned when the MD5 checksum doesn't match the target.
	ErrMD5Mismatch = errors.New("MFsymlink MD5 checksum mismatch")
)

// Encode generates MFsymlink content for the given symlink target.
// Returns a 1067-byte buffer containing the MFsymlink-formatted data.
func Encode(target string) ([]byte, error) {
	if len(target) > MaxTargetLength {
		return nil, ErrTargetTooLong
	}

	if len(target) == 0 {
		return nil, errors.New("symlink target cannot be empty")
	}

	// Calculate MD5 of target
	hash := md5.Sum([]byte(target))
	md5Hex := hex.EncodeToString(hash[:])

	// Build the MFsymlink content
	buf := make([]byte, Size)

	// Magic marker (5 bytes)
	copy(buf[0:5], Magic)

	// Target length (5 bytes): 4-digit zero-padded + newline
	lengthStr := fmt.Sprintf("%04d\n", len(target))
	copy(buf[5:10], lengthStr)

	// MD5 hash (33 bytes): 32 hex chars + newline
	copy(buf[10:42], md5Hex)
	buf[42] = '\n'

	// Target + newline (variable)
	copy(buf[43:43+len(target)], target)
	buf[43+len(target)] = '\n'

	// Padding with spaces to fill remaining bytes
	for i := 44 + len(target); i < Size; i++ {
		buf[i] = ' '
	}

	return buf, nil
}

// Decode parses an MFsymlink and returns the symlink target.
// It validates the magic marker and MD5 checksum.
func Decode(data []byte) (string, error) {
	if len(data) != Size {
		return "", fmt.Errorf("%w: wrong size %d (expected %d)", ErrInvalidMFsymlink, len(data), Size)
	}

	if !IsMFsymlink(data) {
		return "", fmt.Errorf("%w: missing magic marker", ErrInvalidMFsymlink)
	}

	// Parse target length
	lengthStr := string(data[5:9])
	var length int
	_, err := fmt.Sscanf(lengthStr, "%d", &length)
	if err != nil {
		return "", fmt.Errorf("%w: invalid length field", ErrInvalidMFsymlink)
	}

	if length <= 0 || length > MaxTargetLength {
		return "", fmt.Errorf("%w: length %d out of range", ErrInvalidMFsymlink, length)
	}

	// Check newline after length
	if data[9] != '\n' {
		return "", fmt.Errorf("%w: missing newline after length", ErrInvalidMFsymlink)
	}

	// Parse MD5 hash
	md5Hex := string(data[10:42])
	if data[42] != '\n' {
		return "", fmt.Errorf("%w: missing newline after MD5", ErrInvalidMFsymlink)
	}

	// Extract target
	if 43+length > Size {
		return "", fmt.Errorf("%w: length exceeds buffer", ErrInvalidMFsymlink)
	}
	target := string(data[43 : 43+length])

	// Validate MD5
	if !ValidateMD5(data) {
		// Calculate expected MD5 for error message
		hash := md5.Sum([]byte(target))
		expected := hex.EncodeToString(hash[:])
		return "", fmt.Errorf("%w: got %s, expected %s", ErrMD5Mismatch, md5Hex, expected)
	}

	return target, nil
}

// IsMFsymlink checks if the data starts with the MFsymlink magic marker.
// This is a quick check that doesn't validate the entire format.
func IsMFsymlink(data []byte) bool {
	if len(data) < len(Magic) {
		return false
	}
	return bytes.Equal(data[:len(Magic)], []byte(Magic))
}

// ValidateMD5 verifies that the MD5 checksum in the MFsymlink matches the target.
// Returns false if the data is not a valid MFsymlink or the checksum doesn't match.
func ValidateMD5(data []byte) bool {
	if len(data) != Size || !IsMFsymlink(data) {
		return false
	}

	// Parse target length
	lengthStr := string(data[5:9])
	var length int
	_, err := fmt.Sscanf(lengthStr, "%d", &length)
	if err != nil || length <= 0 || length > MaxTargetLength {
		return false
	}

	// Check bounds
	if 43+length > Size {
		return false
	}

	// Extract stored MD5 and target
	storedMD5 := string(data[10:42])
	target := string(data[43 : 43+length])

	// Calculate actual MD5
	hash := md5.Sum([]byte(target))
	actualMD5 := hex.EncodeToString(hash[:])

	return storedMD5 == actualMD5
}

// QuickCheck performs a fast check if a file might be an MFsymlink.
// It checks only size and magic marker, not the full format.
// Use this for filtering before reading the full content.
func QuickCheck(size int64, firstBytes []byte) bool {
	return size == Size && IsMFsymlink(firstBytes)
}
