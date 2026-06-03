// Package handlers implements the SMB2/3 command handlers for DittoFS.
//
// Each handler corresponds to an SMB2 command as defined in [MS-SMB2] and
// translates wire-level requests into operations against the per-share
// metadata service and block store, reached through the share registry.
//
// # Handler
//
// The Handler type is the central coordinator for a server's SMB2 operations.
// Its mutable state uses sync.Map and atomic counters for lock-free concurrent
// access across connections:
//
//   - SessionManager: authenticated sessions and credit tracking.
//   - pendingAuth (sync.Map): sessions mid-NTLM-handshake.
//   - trees (sync.Map) + nextTreeID: tree connections (mounted shares).
//   - files (sync.Map) + nextFileID: open file handles, keyed by 16-byte FileID.
//
// # Session lifecycle
//
// A client connects over TCP, negotiates a dialect (NEGOTIATE), authenticates
// over one or two SESSION_SETUP rounds (NTLM wrapped in SPNEGO; Kerberos when
// configured), connects to a share (TREE_CONNECT), issues file operations, and
// finally tears down with TREE_DISCONNECT and LOGOFF. Pending authentications
// are tracked in pendingAuth until the handshake completes or times out.
//
// # Error handling
//
// Handlers return NT_STATUS codes via HandlerResult. Store errors are mapped to
// NT_STATUS through internal/adapter/common (errmap.go and friends) so the
// mapping stays consistent with the NFS adapters.
//
// # References
//
//   - [MS-SMB2] Server Message Block Protocol Versions 2 and 3:
//     https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2
package handlers
