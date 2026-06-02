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

// ============================================================================
// Shared UTF-16LE Encoding/Decoding Utilities
// ============================================================================
//
// These are the canonical UTF-16LE <-> string converters for the handlers
// package. They are surrogate-safe: well-formed UTF-16 (including correctly
// paired surrogates) round-trips identically, and unpaired surrogates are
// preserved as distinct WTF-8 sequences rather than being collapsed to U+FFFD.
// This matters for SMB filenames, which are arbitrary 16-bit code-unit
// sequences and not required to be well-formed UTF-16 ([MS-SMB2] 2.1) — see
// the detailed rationale on decodeUTF16LESurrogateSafe in converters.go. For
// well-formed input (the share path, volume labels, pipe names, etc.) the
// output is byte-for-byte identical to the lossy stdlib path they replaced.

// encodeUTF16LE converts a Go string to UTF-16LE bytes. It is surrogate-safe:
// see encodeUTF16LESurrogateSafe in converters.go.
//
// SMB2 uses UTF-16LE encoding for all string data (filenames, paths, etc.).
//
// **Example:**
//
//	utf16Bytes := encodeUTF16LE("test.txt")
//	// Returns: []byte{'t', 0, 'e', 0, 's', 0, 't', 0, '.', 0, 't', 0, 'x', 0, 't', 0}
func encodeUTF16LE(s string) []byte {
	return encodeUTF16LESurrogateSafe(s)
}

// decodeUTF16LE converts UTF-16LE bytes to a Go string. It is surrogate-safe:
// see decodeUTF16LESurrogateSafe in converters.go.
//
// SMB2 uses UTF-16LE encoding for all string data (filenames, paths, etc.).
//
// **Example:**
//
//	filename := decodeUTF16LE(body[offset:offset+length])
//	// Converts UTF-16LE encoded filename to Go string
//
// **Notes:**
//   - If input has odd length, the last byte is ignored.
//   - Unpaired surrogates are preserved (WTF-8), so two distinct lone
//     surrogates do not alias — required by smb2.charset.Testing.
func decodeUTF16LE(b []byte) string {
	return decodeUTF16LESurrogateSafe(b)
}
