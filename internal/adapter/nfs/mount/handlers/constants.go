package handlers

// Mount Protocol Procedure Numbers
//
// The Mount protocol is a separate RPC service (program 100005, version 3) that works
// alongside NFS to manage the mounting of remote file systems. It provides access control
// and returns the initial file handle needed to access an exported directory.
//
// Key characteristics:
// - All procedures use XDR encoding for requests and responses
// - Most procedures are idempotent (UMNT, UMNTALL may have side effects)
// - The protocol is stateful (maintains mount list) but loosely coupled
// - Authentication is handled via RPC AUTH flavors (returned in MNT response)
//
// Reference: RFC 1813 Appendix I (Mount Protocol Definition)
const (
	// MountProcNull performs no operation and is used to test connectivity.
	//
	// This procedure does nothing and returns nothing. It is primarily used by
	// clients to verify that the Mount service is available and responding.
	//
	// Request:  void
	// Response: void
	// Errors:   None (always succeeds if server is reachable)
	//
	// Usage: Called during initial discovery or health checks
	MountProcNull = 0

	// MountProcMnt mounts an exported directory and returns its file handle.
	//
	// This is the primary Mount protocol procedure. It performs several critical tasks:
	// 1. Validates that the requested path is exported by the server
	// 2. Checks client authorization against export access controls
	// 3. Returns the root file handle for the exported directory
	// 4. Lists supported authentication flavors for this export
	// 5. Records the mount in the server's mount table (for DUMP procedure)
	//
	// The returned file handle becomes the entry point for all subsequent NFS
	// operations on this mount. Clients must use LOOKUP to navigate below this point.
	//
	// Request:  dirpath (variable-length string, max ~1024 bytes)
	//           - Path to the exported directory (e.g., "/export/data")
	//           - Path matching is typically case-sensitive and exact
	//           - May include trailing slashes (implementation-dependent)
	//
	// Response: fhstatus3 structure containing:
	//           - status: Mount status code (see MountOK, MountErrAccess, etc.)
	//           - fhandle: NFS file handle (opaque, typically 32-64 bytes)
	//           - auth_flavors: List of supported RPC authentication types
	//             (e.g., AUTH_UNIX, AUTH_DH, AUTH_KERB)
	//
	// Common error scenarios:
	// - MountErrNoEnt: Export path does not exist
	// - MountErrAccess: Client IP/hostname not in export's allowed list
	// - MountErrNotDir: Export path exists but is not a directory
	// - MountErrIO: Server cannot read export configuration
	//
	// Security considerations:
	// - This is the primary access control point for NFS exports
	// - Server must validate client identity (hostname/IP) against export rules
	// - Should implement rate limiting to prevent brute-force export enumeration
	// - Consider logging all mount attempts for security auditing
	//
	// Reference: RFC 1813 Appendix I.1
	MountProcMnt = 1

	// MountProcDump returns the list of current mounts on the server.
	//
	// This procedure returns all active mount entries that have been created via
	// MNT and not yet removed via UMNT or UMNTALL. The mount list is maintained
	// by the server but is advisory only - it does not affect actual access control.
	//
	// The returned list is primarily used for:
	// - Monitoring which clients have mounted which exports
	// - Administrative tools (e.g., "showmount -a" command)
	// - Graceful shutdown coordination (server can notify active clients)
	//
	// Request:  void
	//
	// Response: mountlist - A linked list where each entry contains:
	//           - hostname: Client that performed the mount (string)
	//           - directory: Export path that was mounted (string)
	//           - next: Pointer to next entry (or null)
	//
	// Implementation notes:
	// - List may be truncated if too many mounts exist
	// - Stale entries may persist if clients crash without calling UMNT
	// - List is not authoritative for access control decisions
	// - Some implementations prune old entries via timeout or restart
	//
	// Privacy considerations:
	// - May expose information about network topology and client activity
	// - Consider restricting to trusted networks or authenticated callers
	//
	// Reference: RFC 1813 Appendix I.2
	MountProcDump = 2

	// MountProcUmnt removes a specific mount entry from the server's mount table.
	//
	// This procedure removes a single mount entry created by a previous MNT call.
	// It is advisory only - removal does not revoke access or invalidate file handles.
	// Clients continue to have access to the mounted export until file handles expire
	// or the server is restarted.
	//
	// Well-behaved clients should call UMNT when unmounting a file system to:
	// - Keep the server's mount table accurate
	// - Reduce server state/memory usage
	// - Support clean shutdown protocols
	//
	// Request:  dirpath (string)
	//           - Same export path that was provided to MNT
	//           - Must match exactly (case-sensitive)
	//
	// Response: void
	//           - No return value or error status
	//           - Always succeeds even if path was not mounted
	//           - Idempotent (safe to call multiple times)
	//
	// Implementation notes:
	// - Server should match both dirpath and client identifier (IP/hostname)
	// - May remove only the first matching entry if multiple exist
	// - Does not affect active NFS operations using existing file handles
	// - Failure to call UMNT has no functional impact on NFS access
	//
	// Reference: RFC 1813 Appendix I.3
	MountProcUmnt = 3

	// MountProcUmntAll removes all mount entries for the calling client.
	//
	// This procedure removes all mount entries from the server's mount table that
	// were created by the calling client. Like UMNT, it is advisory only and does
	// not affect active NFS operations or revoke access.
	//
	// Typical usage scenarios:
	// - Client system shutdown (unmount all NFS file systems at once)
	// - Administrative cleanup operations
	// - Client recovery after crash or network partition
	//
	// Request:  void
	//
	// Response: void
	//           - No return value or error status
	//           - Always succeeds even if client has no mounts
	//           - Idempotent (safe to call multiple times)
	//
	// Implementation notes:
	// - Server identifies client by RPC credentials (IP address, hostname)
	// - Removes all entries matching the client identifier
	// - Does not affect other clients' mount entries
	// - Does not invalidate existing file handles or active operations
	//
	// Reference: RFC 1813 Appendix I.4
	MountProcUmntAll = 4

	// MountProcExport returns the server's list of exported file systems.
	//
	// This procedure allows clients to discover which directories are available
	// for mounting without attempting to mount them. It is the Mount protocol
	// equivalent of the NFS FSINFO procedure - used for discovery and capability
	// negotiation.
	//
	// The export list includes:
	// - Path to each exported directory
	// - Optional list of client groups/netgroups allowed to mount
	// - Access control information (read-only vs. read-write)
	//
	// Typical usage:
	// - "showmount -e server" command
	// - Automounter configuration discovery
	// - Client-side mount wizards and GUI tools
	//
	// Request:  void
	//
	// Response: exports - A linked list where each entry contains:
	//           - ex_dir: Export directory path (string)
	//           - ex_groups: Linked list of allowed client groups (strings)
	//             - May be empty (world-accessible) or null
	//             - Can include hostnames, IP addresses, or netgroup names
	//           - next: Pointer to next export entry (or null)
	//
	// Security considerations:
	// - May expose server directory structure to unauthorized clients
	// - Some implementations restrict EXPORT to trusted networks
	// - Consider rate limiting to prevent reconnaissance
	// - May want to filter results based on caller's identity
	//
	// Implementation notes:
	// - Export list is typically read from /etc/exports or similar config
	// - List may be cached and not reflect real-time configuration changes
	// - Response size can grow large with many exports (may need truncation)
	// - Access control groups are hints only (actual enforcement happens in MNT)
	//
	// Reference: RFC 1813 Appendix I.5
	MountProcExport = 5
)

// Mount Protocol Status Codes
//
// These status codes are returned exclusively by the MountProcMnt procedure to indicate
// success or failure of mount operations. Other Mount procedures (NULL, DUMP, UMNT,
// UMNTALL, EXPORT) do not return status codes - they return void or data structures.
//
// The status codes follow Unix errno conventions where applicable, making them familiar
// to developers and allowing for consistent error handling across POSIX-like systems.
// This alignment with errno values is intentional but not required by the RFC.
//
// Important implementation notes:
// - Clients must check status before attempting to use the returned file handle
// - Non-zero status indicates mount failure; file handle and auth list are invalid
// - Servers should log mount failures for security auditing
// - Status codes should be mapped to user-friendly error messages
//
// Reference: RFC 1813 Appendix I.1 (fhstatus3 definition)
const (
	// MountOK indicates successful mount operation.
	//
	// When MountOK (0) is returned:
	// - The file handle is valid and can be used for NFS operations
	// - The auth_flavors list contains at least one supported authentication method
	// - The mount entry has been added to the server's mount table
	// - Client can proceed with NFS LOOKUP and other file operations
	//
	// Clients should validate:
	// - File handle is non-empty
	// - At least one auth flavor is supported by both client and server
	// - First NFS operation (typically GETATTR or LOOKUP) succeeds
	MountOK = 0

	// MountErrPerm indicates operation not permitted.
	//
	// This status is returned when the mount operation itself is not allowed,
	// typically due to:
	// - Export is marked as "no_root_squash" but client is trying root access
	// - Export has specific user/group restrictions that client doesn't meet
	// - Mount daemon security policy prevents the operation
	//
	// This differs from MountErrAccess which indicates network-level denial.
	//
	// Corresponds to: Unix errno EPERM (1)
	MountErrPerm = 1

	// MountErrNoEnt indicates export path not found.
	//
	// Returned when:
	// - Requested export path does not exist in server's export list
	// - Export path exists but has been removed from configuration
	// - Path has not been explicitly exported (even if it exists on disk)
	//
	// Common causes:
	// - Typo in export path
	// - Export configuration not loaded or out of date
	// - Export path was removed from /etc/exports
	// - Client using wrong server hostname/IP
	//
	// Client should:
	// - Verify export path spelling
	// - Use EXPORT procedure to list available exports
	// - Check with system administrator about export configuration
	//
	// Corresponds to: Unix errno ENOENT (2)
	MountErrNoEnt = 2

	// MountErrIO indicates I/O error reading export configuration.
	//
	// Returned when server encounters an internal error:
	// - Cannot read export configuration file (/etc/exports)
	// - Database or backing store unavailable
	// - Disk I/O error accessing export metadata
	// - Corruption in export configuration data
	//
	// This is a server-side error that clients cannot resolve. The client should:
	// - Log the error for administrator attention
	// - Retry after a delay (issue may be transient)
	// - Try alternate servers if available (in clustered setups)
	//
	// Server should:
	// - Log detailed error information for debugging
	// - Check file system and configuration file integrity
	// - Consider health check failure if this persists
	//
	// Corresponds to: Unix errno EIO (5)
	MountErrIO = 5

	// MountErrAccess indicates client not authorized to mount.
	//
	// This is the most common mount failure and indicates that:
	// - Client's IP address or hostname is not in the export's allowed list
	// - Export is restricted to specific netgroups that don't include this client
	// - Export has "noaccess" or similar restriction for this client
	// - Hostname reverse DNS lookup failed or doesn't match forward lookup
	//
	// Security implications:
	// - This is the primary access control enforcement point
	// - Servers should log these attempts for security monitoring
	// - Repeated failures may indicate scanning/attack attempts
	// - Rate limiting should be applied to prevent brute force
	//
	// Common causes:
	// - Client not added to /etc/exports allowed list
	// - DNS configuration issues (reverse lookup mismatch)
	// - IP address change not reflected in export rules
	// - Firewall or network policy blocking access
	//
	// Client should:
	// - Verify network configuration and DNS resolution
	// - Contact administrator to be added to export access list
	// - Check if using correct network interface/IP
	//
	// Corresponds to: Unix errno EACCES (13)
	MountErrAccess = 13

	// MountErrNotDir indicates export path exists but is not a directory.
	//
	// Returned when:
	// - Export path points to a regular file instead of directory
	// - Export path is a symlink to a non-directory
	// - Export path is a special file (device, socket, etc.)
	//
	// The Mount protocol requires all exports to be directories since the
	// returned file handle becomes the root of a file system namespace.
	// Individual files cannot be mounted.
	//
	// This typically indicates:
	// - Configuration error in /etc/exports
	// - File system change after export was configured
	// - Incorrect path in client mount command
	//
	// Server should:
	// - Validate export paths are directories at configuration load time
	// - Log this error as potential configuration issue
	//
	// Corresponds to: Unix errno ENOTDIR (20)
	MountErrNotDir = 20

	// MountErrInval indicates invalid argument in mount request.
	//
	// Returned when:
	// - Export path is malformed (invalid characters, encoding)
	// - Export path is empty string
	// - Path contains null bytes or control characters
	// - Path exceeds maximum length before name-too-long check
	// - Request has invalid XDR encoding
	// - Protocol version mismatch
	//
	// This indicates a client bug or malicious request. Server should:
	// - Log the invalid request for security monitoring
	// - Include details about what was invalid (for debugging)
	// - Consider rate limiting source of invalid requests
	//
	// Client should:
	// - Validate input before sending to server
	// - Check protocol version compatibility
	// - Report as bug if client believes request is valid
	//
	// Corresponds to: Unix errno EINVAL (22)
	MountErrInval = 22

	// MountErrNameTooLong indicates export path exceeds maximum length.
	//
	// Returned when:
	// - Export path string exceeds implementation limits (typically 1024 bytes)
	// - Path would exceed MAXPATHLEN in server's file system
	// - Individual path component exceeds MAXNAMLEN (typically 255 bytes)
	//
	// Limits vary by implementation but common values:
	// - Total path: 1024 or 4096 bytes
	// - Path component: 255 bytes
	// - NFS protocol limit: Typically 1024 bytes for dirpath
	//
	// Client should:
	// - Check path length before attempting mount
	// - Consider using shorter export paths or symlinks
	// - Report to administrator if legitimate paths exceed limits
	//
	// Corresponds to: Unix errno ENAMETOOLONG (63 on Linux, 78 on BSD)
	MountErrNameTooLong = 63

	// MountErrNotSupp indicates operation not supported.
	//
	// Returned when:
	// - Server does not support the requested mount protocol version
	// - Mount features required by client are not available
	// - Export configuration specifies unsupported options
	// - Protocol procedure is recognized but not implemented
	//
	// This is typically a configuration or compatibility issue:
	// - Client requesting NFSv4 features over NFSv3 mount protocol
	// - Export options that server doesn't support
	// - Deprecated features that have been removed
	//
	// Client should:
	// - Check protocol version compatibility
	// - Verify server capabilities before mounting
	// - Consider fallback to older protocol versions
	//
	// Corresponds to: NFS3ERR_NOTSUPP (10004)
	// Note: This uses NFS error space, not Unix errno space
	MountErrNotSupp = 10004

	// MountErrServerFault indicates internal server error.
	//
	// This is a catch-all for unexpected server failures that don't fit other
	// categories:
	// - Unhandled exception or panic in mount code
	// - Memory allocation failure
	// - Internal state corruption
	// - Unexpected condition that should never occur
	// - Resource exhaustion (too many mounts, out of memory)
	//
	// This indicates a server bug or serious resource problem. Unlike MountErrIO
	// which is specific to configuration I/O, this is a general internal error.
	//
	// Server should:
	// - Log detailed error with stack trace
	// - Include internal error details for debugging
	// - Consider health check failure
	// - Alert administrators if this occurs frequently
	//
	// Client should:
	// - Treat as transient and retry with backoff
	// - Log for administrator attention
	// - Try alternate servers if available
	// - Escalate if problem persists
	//
	// Corresponds to: NFS3ERR_SERVERFAULT (10006)
	// Note: This uses NFS error space, not Unix errno space
	MountErrServerFault = 10006
)
