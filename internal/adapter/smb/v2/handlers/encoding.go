// Package handlers provides SMB2 command handlers and session management.
//
// This file contains shared UTF-16LE encoding/decoding utilities used by
// multiple SMB2 handlers. SMB2 uses UTF-16LE for all string data (filenames,
// paths, share names, etc.).
//
// All handler-specific encoding/decoding functions have been moved to their
// respective handler files:
//   - FlushRequest/Response encoding -> flush.go
//   - CloseRequest/Response encoding -> close.go
//   - ReadRequest/Response encoding -> read.go
//   - WriteRequest/Response encoding -> write.go
//   - CreateRequest/Response encoding -> create.go
//   - QueryInfoRequest/Response encoding -> query_info.go
//   - SetInfoRequest/Response encoding -> set_info.go
//   - QueryDirectoryRequest/Response encoding -> query_directory.go
package handlers

import (
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
)

// ============================================================================
// Shared UTF-16LE Encoding/Decoding Utilities
// ============================================================================

// encodeUTF16LE converts a Go string to UTF-16LE bytes.
//
// SMB2 uses UTF-16LE encoding for all string data (filenames, paths, etc.).
// This function handles the conversion from Go's internal UTF-8 representation.
//
// **Process:**
//  1. Convert Go string (UTF-8) to runes
//  2. Encode runes to UTF-16 code units
//  3. Convert each 16-bit value to little-endian byte order
//
// **Example:**
//
//	utf16Bytes := encodeUTF16LE("test.txt")
//	// Returns: []byte{'t', 0, 'e', 0, 's', 0, 't', 0, '.', 0, 't', 0, 'x', 0, 't', 0}
//
// **Parameters:**
//   - s: Go string to encode
//
// **Returns:**
//   - []byte: UTF-16LE encoded bytes (length = 2 * number of UTF-16 code units)
func encodeUTF16LE(s string) []byte {
	runes := []rune(s)
	encoded := utf16.Encode(runes)
	w := smbenc.NewWriter(len(encoded) * 2)
	for _, r := range encoded {
		w.WriteUint16(r)
	}
	return w.Bytes()
}

// decodeUTF16LE converts UTF-16LE bytes to a Go string.
//
// SMB2 uses UTF-16LE encoding for all string data (filenames, paths, etc.).
// This function handles the conversion to Go's internal UTF-8 representation.
//
// **Process:**
//  1. Pair bytes into 16-bit values (little-endian)
//  2. Decode UTF-16 code units to Unicode code points
//  3. Convert to Go string (UTF-8)
//
// **Example:**
//
//	filename := decodeUTF16LE(body[offset:offset+length])
//	// Converts UTF-16LE encoded filename to Go string
//
// **Parameters:**
//   - b: UTF-16LE encoded bytes
//
// **Returns:**
//   - string: Decoded Go string (UTF-8)
//
// **Notes:**
//   - If input has odd length, the last byte is ignored
//   - Invalid UTF-16 sequences are replaced with U+FFFD
func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1] // Truncate odd byte
	}
	r := smbenc.NewReader(b)
	u16s := make([]uint16, len(b)/2)
	for i := range u16s {
		u16s[i] = r.ReadUint16()
	}
	return string(utf16.Decode(u16s))
}
