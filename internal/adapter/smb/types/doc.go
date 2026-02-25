// Package types contains SMB2 protocol constants, types, and error codes.
//
// # Overview
//
// This package provides type-safe definitions for SMB2 protocol elements:
//
//   - Command codes (NEGOTIATE, SESSION_SETUP, CREATE, READ, WRITE, etc.)
//   - Header flags (response, async, signed, related operations)
//   - Dialects (SMB 2.0.2, 2.1, 3.0, 3.0.2, 3.1.1)
//   - NT_STATUS error codes
//   - File attributes, access masks, and create options
//   - FILETIME conversion utilities
//
// # Type Safety
//
// All types use explicit Go types (e.g., Command, HeaderFlags, Status) to:
//
//   - Enable IDE autocomplete
//   - Prevent mixing incompatible values
//   - Provide human-readable string representations
//
// # Command Codes
//
// SMB2 defines 19 commands (0x0000-0x0012):
//
//	const (
//	    CommandNegotiate      Command = 0x0000  // Protocol negotiation
//	    CommandSessionSetup   Command = 0x0001  // Authentication
//	    CommandLogoff         Command = 0x0002  // Session termination
//	    CommandTreeConnect    Command = 0x0003  // Connect to share
//	    CommandTreeDisconnect Command = 0x0004  // Disconnect from share
//	    CommandCreate         Command = 0x0005  // Open/create file
//	    CommandClose          Command = 0x0006  // Close file handle
//	    CommandFlush          Command = 0x0007  // Flush to disk
//	    CommandRead           Command = 0x0008  // Read data
//	    CommandWrite          Command = 0x0009  // Write data
//	    // ... etc
//	)
//
// # NT_STATUS Codes
//
// Windows error codes are 32-bit values:
//
//	Bits 31-30: Severity (00=Success, 01=Info, 10=Warning, 11=Error)
//	Bit 27:     Customer code flag
//	Bits 16-26: Facility code
//	Bits 0-15:  Error code
//
// Common status codes:
//
//	StatusSuccess                   = 0x00000000  // OK
//	StatusMoreProcessingRequired    = 0xC0000016  // Continue auth
//	StatusAccessDenied              = 0xC0000022  // Permission denied
//	StatusObjectNameNotFound        = 0xC0000034  // File not found
//	StatusObjectNameCollision       = 0xC0000035  // File exists
//
// # FILETIME
//
// Windows timestamps use 100-nanosecond intervals since January 1, 1601:
//
//	// Convert Unix time to FILETIME
//	filetime := UnixToFiletime(time.Now().Unix())
//
//	// Convert FILETIME to Unix time
//	unixTime := FiletimeToUnix(filetime)
//
// # File Attributes
//
// File attributes are a bitmask:
//
//	FileAttributeReadonly   = 0x00000001  // Read-only file
//	FileAttributeHidden     = 0x00000002  // Hidden file
//	FileAttributeDirectory  = 0x00000010  // Directory
//	FileAttributeNormal     = 0x00000080  // Normal file
//
// # References
//
//   - [MS-SMB2] Server Message Block Protocol Versions 2 and 3
//   - [MS-ERREF] Windows Error Codes
//   - [MS-FSCC] File System Control Codes
package types
