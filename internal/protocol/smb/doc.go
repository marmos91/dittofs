// Package smb implements a production-ready SMB2 server over TCP.
//
// # Architecture Overview
//
// The SMB implementation follows a layered architecture with clear separation of concerns:
//
//   - Adapter Layer (pkg/adapter/smb/): Connection management, lifecycle, configuration
//   - Dispatch Layer (dispatch.go): Command routing and handler invocation
//   - Header Layer (header/): SMB2 header parsing and encoding
//   - Handler Layer (v2/handlers/): Protocol-specific business logic
//   - Session Layer (session/): Session and credit management
//   - Types Layer (types/): Protocol constants and type definitions
//
// # Protocol Support
//
// DittoFS implements SMB2 dialect 0x0202 (SMB 2.0.2):
//
//   - Session management: NEGOTIATE, SESSION_SETUP, LOGOFF
//   - Share access: TREE_CONNECT, TREE_DISCONNECT
//   - File operations: CREATE, CLOSE, READ, WRITE, FLUSH
//   - Directory operations: QUERY_DIRECTORY
//   - Metadata: QUERY_INFO, SET_INFO
//   - Utility: ECHO
//
// # Thread Safety
//
// The SMB implementation is designed for safe concurrent operation:
//
//   - All public methods are thread-safe and can be called concurrently
//   - Session and tree state use sync.Map for lock-free access
//   - ID generation uses atomic operations
//   - Credit management uses atomic counters
//
// # Authentication
//
// SMB2 authentication uses NTLM wrapped in SPNEGO:
//
//  1. Client sends NEGOTIATE → Server returns capabilities
//  2. Client sends SESSION_SETUP with NTLM Type 1 → Server returns Type 2 (CHALLENGE)
//  3. Client sends SESSION_SETUP with NTLM Type 3 → Server validates and creates session
//
// Guest authentication is supported when credentials are invalid or not provided.
//
// # Credits System
//
// SMB2 uses credit-based flow control to prevent server overload:
//
//   - Clients request credits in each message
//   - Server grants credits in responses
//   - Each request consumes credits (usually 1, more for large I/O)
//   - Three strategies available: fixed, echo, adaptive (default)
//
// # Cross-Protocol Behavior
//
// DittoFS bridges Unix/NFS and Windows/SMB conventions:
//
//   - Hidden files: Dot-prefix files get FILE_ATTRIBUTE_HIDDEN
//   - Special files: FIFO/socket/device nodes hidden from SMB listings
//   - Symlinks: MFsymlink format automatically converted on CLOSE
//
// # Key Types
//
//   - Command: SMB2 command metadata with handler and requirements
//   - HandlerResult: Response from command handlers
//   - DispatchTable: Maps command codes to handlers
//
// # References
//
//   - [MS-SMB2] Server Message Block Protocol Versions 2 and 3
//     https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/
//   - [MS-NLMP] NT LAN Manager (NTLM) Authentication Protocol
//     https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/
//   - [MS-ERREF] Windows Error Codes
//     https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/
package smb
