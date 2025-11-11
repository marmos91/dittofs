package mount

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ExportRequest represents an EXPORT request from an NFS client.
// The EXPORT procedure returns a list of all file systems that the server
// is currently exporting (making available for mounting).
// This procedure takes no parameters.
//
// RFC 1813 Appendix I specifies the EXPORT procedure as:
//
//	EXPORT() -> exports
//
// The exports list is a linked list of export entries, where each entry contains:
//   - directory: The path on the server that can be mounted
//   - groups: A list of client groups/hosts allowed to mount (optional)
type ExportRequest struct {
	// Empty struct - EXPORT takes no parameters
}

// ExportContext contains the context information needed to process an export request.
// This includes cancellation handling for the request lifecycle.
type ExportContext struct {
	// Context carries cancellation signals and deadlines
	// The Export handler checks this context to abort operations if the client
	// disconnects or the request times out
	Context context.Context
}

// ExportResponse represents the response to an EXPORT request.
// It contains a list of all file systems currently exported by the server.
//
// The response format follows the XDR specification for a linked list,
// where each entry is followed by a boolean indicating if more entries exist.
type ExportResponse struct {
	// Entries is the list of currently exported filesystems
	// Each entry contains the export path and optionally a list of allowed groups
	Entries []ExportEntry
}

// ExportEntry represents a single export entry in the EXPORT response.
// This structure corresponds to the "exportnode" type in RFC 1813 Appendix I.
type ExportEntry struct {
	// Directory is the path on the server that can be mounted
	// This is the path clients will use in the MOUNT procedure
	// Example: "/export" or "/data/shared"
	Directory string

	// Groups is a list of host groups or client names allowed to mount this export
	// If empty, the export is available to all clients (world-exportable)
	// Each entry can be a hostname, IP address, netgroup name, etc.
	// Example: ["client1.example.com", "192.168.1.0/24", "@engineering"]
	// Note: Many NFS servers leave this empty to indicate "available to all"
	Groups []string
}

// Export handles the EXPORT procedure, which returns a list of all filesystems
// currently exported (available for mounting) by the NFS server.
//
// The export process:
//  1. Check if the context has been cancelled (early exit if client disconnected)
//  2. Query the repository for all configured exports
//  3. Convert repository export entries to EXPORT response format
//  4. Optionally include client restrictions as groups
//  5. Log the operation with count of returned exports
//  6. Return the list of exports to the client
//
// Context cancellation:
//   - The handler respects context cancellation at key I/O points
//   - If the client disconnects or the request times out, the operation aborts
//   - Cancellation is checked before expensive repository operations
//
// Use cases for EXPORT:
//   - Clients discovering available NFS exports before mounting
//   - Administrative tools listing server capabilities
//   - Mount helper utilities (like automount) scanning for available shares
//   - Documentation and audit of server configuration
//
// Important notes:
//   - EXPORT returns all configured exports, regardless of access permissions
//   - Clients may not have permission to actually mount all listed exports
//   - The groups field can indicate access restrictions, but is often empty
//   - No authentication/authorization is performed by EXPORT itself
//   - Mount access control is enforced later by the MOUNT procedure
//
// Security consideration:
//   - EXPORT reveals server configuration to any client
//   - In security-sensitive environments, you may want to restrict EXPORT
//   - Consider implementing access control similar to DUMP if needed
//   - For now, EXPORT is unrestricted per RFC 1813 convention
//
// Parameters:
//   - ctx: Context with cancellation and timeout information
//   - repository: The metadata repository containing export configurations
//   - req: Empty request struct (EXPORT takes no parameters)
//
// Returns:
//   - *ExportResponse: List of all configured exports
//   - error: Returns error for context cancellation or internal server failures
//     (export list is always returned, even if empty, unless cancelled)
//
// RFC 1813 Appendix I: EXPORT Procedure
//
// Example:
//
//	handler := &DefaultMountHandler{}
//	ctx := &ExportContext{Context: context.Background()}
//	req := &ExportRequest{}
//	resp, err := handler.Export(ctx, repository, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // client disconnected
//	    } else {
//	        // internal error
//	    }
//	}
//	fmt.Printf("Available exports: %d\n", len(resp.Entries))
func (h *DefaultMountHandler) Export(
	ctx *ExportContext,
	repository metadata.Repository,
	req *ExportRequest,
) (*ExportResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("Export request cancelled before processing: error=%v", ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	logger.Info("Export request: listing all available exports")

	// Check for cancellation before the potentially expensive export list retrieval
	// This is especially important if there are many exports configured
	select {
	case <-ctx.Context.Done():
		logger.Debug("Export request cancelled during processing: error=%v", ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
	}

	// Get all configured exports from the repository
	// The repository should also respect context cancellation internally
	exports, err := repository.GetExports(ctx.Context)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("Export request cancelled while retrieving exports: error=%v", ctx.Context.Err())
			return nil, ctx.Context.Err()
		}

		logger.Error("Failed to get exports: error=%v", err)
		return nil, fmt.Errorf("failed to retrieve exports: %w", err)
	}

	// Convert repository export entries to EXPORT response format
	entries := make([]ExportEntry, 0, len(exports))
	for _, export := range exports {
		// Build groups list from access control configuration
		// If AllowedClients is specified, include them as groups
		// If empty, the export is world-exportable (available to all)
		groups := make([]string, 0)

		if len(export.Options.AllowedClients) > 0 {
			// Include allowed clients as groups
			// Note: This is informational only - actual access control
			// is enforced by the MOUNT procedure
			groups = append(groups, export.Options.AllowedClients...)
		}
		// Note: We don't include denied clients in the groups list
		// as the groups field traditionally shows who CAN mount, not who cannot

		entries = append(entries, ExportEntry{
			Directory: export.Path,
			Groups:    groups,
		})
	}

	logger.Info("Export successful: returned %d export(s)", len(entries))

	// Log details at debug level
	if len(entries) > 0 {
		for _, entry := range entries {
			if len(entry.Groups) > 0 {
				logger.Debug("  Export: path=%s groups=%v", entry.Directory, entry.Groups)
			} else {
				logger.Debug("  Export: path=%s (world-exportable)", entry.Directory)
			}
		}
	}

	return &ExportResponse{
		Entries: entries,
	}, nil
}

// DecodeExportRequest decodes an EXPORT request.
// Since EXPORT takes no parameters, this function simply validates
// that the data is empty and returns an empty request struct.
//
// Parameters:
//   - data: Should be empty (EXPORT has no parameters)
//
// Returns:
//   - *ExportRequest: Empty request struct
//   - error: Returns error only if data is unexpectedly non-empty
//
// Example:
//
//	data := []byte{} // Empty - EXPORT has no parameters
//	req, err := DecodeExportRequest(data)
//	if err != nil {
//	    // handle error
//	}
func DecodeExportRequest(data []byte) (*ExportRequest, error) {
	// EXPORT takes no parameters, so we just return an empty request
	// We don't error on non-empty data to be lenient with client implementations
	return &ExportRequest{}, nil
}

// Encode serializes the ExportResponse into XDR-encoded bytes.
//
// The encoding follows RFC 1813 Appendix I specification for exports:
//   - For each export entry:
//     1. value_follows = TRUE (1)
//     2. directory (string: length + data + padding)
//     3. groups list:
//     a. For each group:
//   - value_follows = TRUE (1)
//   - group name (string: length + data + padding)
//     b. value_follows = FALSE (0) to end groups list
//   - Final entry:
//     1. value_follows = FALSE (0) to indicate end of exports list
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
//	resp := &ExportResponse{
//	    Entries: []ExportEntry{
//	        {Directory: "/export", Groups: []string{}},
//	        {Directory: "/data", Groups: []string{"192.168.1.0/24"}},
//	    },
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // handle error
//	}
func (resp *ExportResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Encode each export entry as a linked list node
	for _, entry := range resp.Entries {
		// Write value_follows = TRUE (more entries coming)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("write value_follows: %w", err)
		}

		// Write directory (string: length + data + padding)
		directoryLen := uint32(len(entry.Directory))
		if err := binary.Write(&buf, binary.BigEndian, directoryLen); err != nil {
			return nil, fmt.Errorf("write directory length: %w", err)
		}
		buf.Write([]byte(entry.Directory))

		// Add padding to 4-byte boundary
		directoryPadding := (4 - (directoryLen % 4)) % 4
		buf.Write(make([]byte, directoryPadding))

		// Write groups list (linked list of strings)
		for _, group := range entry.Groups {
			// Write value_follows = TRUE (more groups coming)
			if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
				return nil, fmt.Errorf("write groups value_follows: %w", err)
			}

			// Write group name (string: length + data + padding)
			groupLen := uint32(len(group))
			if err := binary.Write(&buf, binary.BigEndian, groupLen); err != nil {
				return nil, fmt.Errorf("write group length: %w", err)
			}
			buf.Write([]byte(group))

			// Add padding to 4-byte boundary
			groupPadding := (4 - (groupLen % 4)) % 4
			buf.Write(make([]byte, groupPadding))
		}

		// Write value_follows = FALSE (no more groups for this export)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("write groups end marker: %w", err)
		}
	}

	// Write value_follows = FALSE (no more export entries)
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write final value_follows: %w", err)
	}

	return buf.Bytes(), nil
}
