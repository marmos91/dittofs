package handlers

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Handler processes NLM procedure calls.
//
// Handler holds a reference to the MetadataService for performing lock
// operations. Each NLM procedure method receives an NLMHandlerContext
// containing the RPC call context and authentication information.
//
// Thread Safety:
// Handler is safe for concurrent use by multiple goroutines.
// The underlying MetadataService handles synchronization.
type Handler struct {
	metadataService *metadata.MetadataService
}

// NewHandler creates a new NLM handler with the given metadata service.
//
// Parameters:
//   - metadataService: The metadata service for performing lock operations.
//     Must not be nil.
//
// Returns a configured Handler ready to process NLM requests.
func NewHandler(metadataService *metadata.MetadataService) *Handler {
	return &Handler{
		metadataService: metadataService,
	}
}
