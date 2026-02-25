package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
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
	MountResponseBase // Embeds Status and GetStatus()
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

// Dump handles MOUNT DUMP (RFC 1813 Appendix I, Mount procedure 2).
// Returns a list of all active mount entries (client hostname + export path).
// Delegates to Runtime.ListMounts to enumerate current mount tracking state.
// No side effects; read-only operation, no authentication required.
// Errors: context cancellation only (DUMP always succeeds otherwise).
func (h *Handler) Dump(ctx *MountHandlerContext, req *DumpRequest) (*DumpResponse, error) {
	// Check for cancellation before starting any work
	if ctx.isContextCancelled() {
		logger.Debug("Dump request cancelled before processing", "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP from address (remove port)
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("Dump request", "client_ip", clientIP)

	// Get all mounts from registry
	mounts := h.Registry.ListMounts()

	// Convert to response format
	entries := make([]DumpEntry, 0, len(mounts))
	for _, mount := range mounts {
		entries = append(entries, DumpEntry{
			Hostname:  mount.ClientAddr,
			Directory: mount.ShareName,
		})
	}

	logger.Info("Dump successful", "client_ip", clientIP, "returned", len(entries))

	// Log details at debug level
	if len(entries) > 0 {
		for _, entry := range entries {
			logger.Debug("Active mount", "client", entry.Hostname, "share", entry.Directory)
		}
	}

	return &DumpResponse{
		MountResponseBase: MountResponseBase{Status: MountOK},
		Entries:           entries,
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
		if err := xdr.WriteXDRString(&buf, entry.Hostname); err != nil {
			return nil, fmt.Errorf("write hostname: %w", err)
		}

		// Write directory (string: length + data + padding)
		if err := xdr.WriteXDRString(&buf, entry.Directory); err != nil {
			return nil, fmt.Errorf("write directory: %w", err)
		}
	}

	// Write value_follows = FALSE (no more entries)
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write final value_follows: %w", err)
	}

	return buf.Bytes(), nil
}
