package mount

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// DumpRequest represents a DUMP request from an NFS client.
// The DUMP procedure returns information about all currently mounted filesystems.
// This procedure takes no parameters.
//
// RFC 1813 Appendix I specifies the DUMP procedure as:
//
//	DUMP() -> mountlist
//
// The mountlist is a linked list of mount entries, where each entry contains:
//   - hostname: The name/address of the client that mounted the filesystem
//   - directory: The path that was mounted
type DumpRequest struct {
	// Empty struct - DUMP takes no parameters
}

// DumpResponse represents the response to a DUMP request.
// It contains a list of all active mount entries on the server.
//
// The response format follows the XDR specification for a linked list,
// where each entry is followed by a boolean indicating if more entries exist.
type DumpResponse struct {
	// Entries is the list of currently active mounts
	// Each entry contains the client hostname/address and mounted directory path
	Entries []DumpEntry
}

// DumpEntry represents a single mount entry in the DUMP response.
// This structure corresponds to the "mountbody" type in RFC 1813 Appendix I.
type DumpEntry struct {
	// Hostname is the name or address of the client that mounted the filesystem
	// Typically this is the IP address or hostname of the NFS client
	// Example: "192.168.1.100" or "client.example.com"
	Hostname string

	// Directory is the path on the server that was mounted
	// This corresponds to the export path provided in the MOUNT request
	// Example: "/export" or "/data/shared"
	Directory string
}

// DumpContext contains the context information needed to process a dump request.
// This includes client identification for access control.
type DumpContext struct {
	Context context.Context
	// ClientAddr is the network address of the client making the request
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string
}

// Dump handles the DUMP procedure, which returns a list of all filesystems
// currently mounted by NFS clients.
//
// The dump process:
//  1. Query the repository for all active mount entries
//  2. Convert repository mount entries to DUMP response format
//  3. Log the operation with count of returned mounts
//  4. Return the list of mounts to the client
//
// Use cases for DUMP:
//   - Administrative monitoring of active NFS clients
//   - Audit logging and security monitoring
//   - Capacity planning (understanding usage patterns)
//   - Troubleshooting client connectivity issues
//   - Preparing for server maintenance (knowing which clients to notify)
//
// Important notes:
//   - DUMP returns all mounts across all exports and clients
//   - The list may be large on busy servers
//   - Mount tracking is maintained by MOUNT/UMNT/UMNTALL procedures
//   - Entries may be stale if clients crashed without unmounting
//   - No authentication/authorization is performed (any client can call DUMP)
//
// Security consideration:
//
//	In a production environment, you may want to restrict DUMP to:
//	- Only local/admin connections
//	- Authenticated clients
//	- Specific IP ranges
//	This can be implemented by adding access control in the repository.
//
// Parameters:
//   - repository: The metadata repository that tracks active mounts
//   - req: Empty request struct (DUMP takes no parameters)
//
// Returns:
//   - *DumpResponse: List of all active mount entries
//   - error: Returns error only for internal server failures
//     (mount list is always returned, even if empty)
//
// RFC 1813 Appendix I: DUMP Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	req := &DumpRequest{}
//	resp, err := handler.Dump(repository, req)
//	if err != nil {
//	    // handle internal error
//	}
//	fmt.Printf("Active mounts: %d\n", len(resp.Entries))
//	for _, entry := range resp.Entries {
//	    fmt.Printf("  %s mounted %s\n", entry.Hostname, entry.Directory)
//	}
func (h *DefaultMountHandler) Dump(ctx *DumpContext, repository metadata.Repository, req *DumpRequest) (*DumpResponse, error) {
	// Extract client IP from address (remove port)
	clientIP, _, err := net.SplitHostPort(ctx.ClientAddr)
	if err != nil {
		// If parsing fails, use the whole address (might be IP only)
		clientIP = ctx.ClientAddr
	}

	logger.Info("Dump request: client=%s", clientIP)

	// Check if client is allowed to call DUMP
	if err := repository.CheckDumpAccess(ctx.Context, clientIP); err != nil {
		if exportErr, ok := err.(*metadata.ExportError); ok {
			logger.Warn("Dump access denied: client=%s reason=%s", clientIP, exportErr.Message)
			// Return empty list instead of error (DUMP has no error status codes)
			// The client gets an empty mount list, which is indistinguishable from
			// a server with no mounts
			return &DumpResponse{Entries: []DumpEntry{}}, nil
		}

		// Unknown error - internal server fault
		logger.Error("Dump access check failed: client=%s error=%v", clientIP, err)
		return nil, fmt.Errorf("failed to check dump access: %w", err)
	}

	// Get all active mounts from the repository
	mounts, err := repository.GetMounts(ctx.Context, "")
	if err != nil {
		logger.Error("Failed to get mounts: error=%v", err)
		return nil, fmt.Errorf("failed to retrieve mounts: %w", err)
	}

	// Convert repository mount entries to DUMP response format
	entries := make([]DumpEntry, 0, len(mounts))
	for _, mount := range mounts {
		entries = append(entries, DumpEntry{
			Hostname:  mount.ClientAddr,
			Directory: mount.ExportPath,
		})
	}

	logger.Info("Dump successful: client=%s returned=%d mount(s)", clientIP, len(entries))

	// Log details at debug level
	if len(entries) > 0 {
		for _, entry := range entries {
			logger.Debug("  Mount: client=%s path=%s", entry.Hostname, entry.Directory)
		}
	}

	return &DumpResponse{
		Entries: entries,
	}, nil
}

// DecodeDumpRequest decodes a DUMP request.
// Since DUMP takes no parameters, this function simply validates
// that the data is empty and returns an empty request struct.
//
// Parameters:
//   - data: Should be empty (DUMP has no parameters)
//
// Returns:
//   - *DumpRequest: Empty request struct
//   - error: Returns error only if data is unexpectedly non-empty
//
// Example:
//
//	data := []byte{} // Empty - DUMP has no parameters
//	req, err := DecodeDumpRequest(data)
//	if err != nil {
//	    // handle error
//	}
func DecodeDumpRequest(data []byte) (*DumpRequest, error) {
	// DUMP takes no parameters, so we just return an empty request
	// We don't error on non-empty data to be lenient with client implementations
	return &DumpRequest{}, nil
}

// Encode serializes the DumpResponse into XDR-encoded bytes.
//
// The encoding follows RFC 1813 Appendix I specification for mountlist:
//   - For each entry:
//     1. value_follows = TRUE (1)
//     2. hostname (string: length + data + padding)
//     3. directory (string: length + data + padding)
//   - Final entry:
//     1. value_follows = FALSE (0) to indicate end of list
//
// XDR encoding requires all data to be aligned to 4-byte boundaries,
// so padding bytes are added after variable-length strings.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &DumpResponse{
//	    Entries: []DumpEntry{
//	        {Hostname: "192.168.1.100", Directory: "/export"},
//	        {Hostname: "192.168.1.101", Directory: "/data"},
//	    },
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // handle error
//	}
func (resp *DumpResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Encode each mount entry as a linked list node
	for _, entry := range resp.Entries {
		// Write value_follows = TRUE (more entries coming)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write value_follows: %w", err)
		}

		// Write hostname (string: length + data + padding)
		hostnameLen := uint32(len(entry.Hostname))
		if err := binary.Write(&buf, binary.BigEndian, hostnameLen); err != nil {
			return nil, fmt.Errorf("write hostname length: %w", err)
		}
		buf.Write([]byte(entry.Hostname))

		// Add padding to 4-byte boundary
		hostnamePadding := (4 - (hostnameLen % 4)) % 4
		buf.Write(make([]byte, hostnamePadding))

		// Write directory (string: length + data + padding)
		directoryLen := uint32(len(entry.Directory))
		if err := binary.Write(&buf, binary.BigEndian, directoryLen); err != nil {
			return nil, fmt.Errorf("write directory length: %w", err)
		}
		buf.Write([]byte(entry.Directory))

		// Add padding to 4-byte boundary
		directoryPadding := (4 - (directoryLen % 4)) % 4
		buf.Write(make([]byte, directoryPadding))
	}

	// Write value_follows = FALSE (no more entries)
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write final value_follows: %w", err)
	}

	return buf.Bytes(), nil
}
