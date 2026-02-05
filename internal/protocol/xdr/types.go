// Package xdr provides generic XDR (External Data Representation) encoding and
// decoding utilities per RFC 4506.
//
// XDR is the standard data serialization format used by Sun RPC protocols
// including NFS and NLM. This package provides protocol-agnostic utilities
// that can be shared across multiple protocol implementations.
//
// Key characteristics of XDR:
//   - Big-endian byte order for all multi-byte integers
//   - 4-byte alignment for all data types
//   - Variable-length data is preceded by a 4-byte length
//   - Strings and opaque data are padded to 4-byte boundaries
//
// This package contains only generic utilities with no dependencies on
// DittoFS-specific packages (no logger, metadata, or protocol types).
//
// Reference: RFC 4506 - XDR: External Data Representation Standard
// https://tools.ietf.org/html/rfc4506
package xdr
