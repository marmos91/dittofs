package handlers

import (
	"bytes"

	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeNullRequest decodes a NULL request from XDR-encoded bytes.
//
// The NULL procedure takes no arguments, so the request body should be empty.
// However, we still provide this function for consistency with other procedures
// and to handle any unexpected data gracefully.
//
// Per RFC 1813, the NULL request has no parameters:
//
//	void NFSPROC3_NULL(void) = 0;
//
// Parameters:
//   - data: XDR-encoded bytes (should be empty for NULL)
//
// Returns:
//   - *NullRequest: Empty request structure
//   - error: Only if data is malformed (non-empty when it should be empty)
//
// Example:
//
//	data := []byte{} // Empty XDR data for NULL request
//	req, err := DecodeNullRequest(data)
//	if err != nil {
//	    // Handle decode error (should be rare for NULL)
//	    return nil, err
//	}
//	// req is an empty NullRequest structure
func DecodeNullRequest(data []byte) (*NullRequest, error) {
	// NULL takes no arguments - data should be empty
	// We accept empty or minimal data for compatibility
	if len(data) > 0 {
		logger.Debug("NULL: received non-empty request body, ignoring", "bytes", len(data))
	}

	return &NullRequest{}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the NullResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The NULL procedure returns no data per RFC 1813, so the response is empty.
// However, we still follow XDR encoding conventions for consistency.
//
// Per RFC 1813, the NULL response has no return value:
//
//	void NFSPROC3_NULL(void) = 0;
//
// Returns:
//   - []byte: Empty byte array (XDR-encoded NULL response)
//   - error: Should never return error for NULL encoding
//
// Example:
//
//	resp := &NullResponse{}
//	data, err := resp.Encode()
//	if err != nil {
//	    // This should never happen
//	    return nil, err
//	}
//	// data is an empty byte array
//	// Send 'data' to client over network
func (resp *NullResponse) Encode() ([]byte, error) {
	// NULL returns no data - return empty buffer
	var buf bytes.Buffer

	logger.Debug("Encoded NULL response", "bytes", 0)
	return buf.Bytes(), nil
}
