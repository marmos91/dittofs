// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// This file implements named pipe state management for RPC communication.
package rpc

import (
	"bytes"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// =============================================================================
// Pipe Handler Interface
// =============================================================================

// PipeHandler is the interface that all named pipe RPC handlers must implement.
// Both SRVSVCHandler and LSARPCHandler satisfy this interface.
type PipeHandler interface {
	HandleBind(req *BindRequest) []byte
	HandleRequest(req *Request) []byte
}

// =============================================================================
// Named Pipe State
// =============================================================================

// PipeState represents the state of a named pipe connection
type PipeState struct {
	mu         sync.Mutex
	Name       string       // Pipe name (e.g., "srvsvc", "lsarpc")
	Bound      bool         // Whether RPC bind has completed
	Handler    PipeHandler  // RPC handler for this pipe
	ReadBuffer *bytes.Buffer // Buffered response data for READ
}

// NewPipeState creates a new pipe state
func NewPipeState(name string, handler PipeHandler) *PipeState {
	return &PipeState{
		Name:       name,
		Handler:    handler,
		ReadBuffer: bytes.NewBuffer(nil),
	}
}

// ProcessWrite handles a WRITE to the named pipe (client -> server)
// Returns data to be made available for subsequent READ
func (p *PipeState) ProcessWrite(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(data) < HeaderSize {
		return nil // Ignore short writes
	}

	hdr, err := ParseHeader(data)
	if err != nil {
		return err
	}

	var response []byte

	switch hdr.PacketType {
	case PDUBind:
		// Handle RPC bind
		bindReq, err := ParseBindRequest(data)
		if err != nil {
			return err
		}
		response = p.Handler.HandleBind(bindReq)
		p.Bound = true

	case PDURequest:
		// Handle RPC request
		if !p.Bound {
			// Not bound yet, ignore
			return nil
		}
		rpcReq, err := ParseRequest(data)
		if err != nil {
			return err
		}
		response = p.Handler.HandleRequest(rpcReq)
	}

	// Buffer response for subsequent READ
	if len(response) > 0 {
		p.ReadBuffer.Write(response)
	}

	return nil
}

// ProcessRead handles a READ from the named pipe (server -> client)
// Returns buffered response data
func (p *PipeState) ProcessRead(maxLen int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ReadBuffer.Len() == 0 {
		return nil
	}

	// Validate maxLen to prevent allocation issues
	if maxLen <= 0 {
		return nil
	}
	// Cap at reasonable maximum (64KB matches SMB protocol limits)
	const maxReadSize = 65536
	if maxLen > maxReadSize {
		maxLen = maxReadSize
	}

	// Read up to maxLen bytes
	data := make([]byte, maxLen)
	// bytes.Buffer.Read only returns io.EOF when empty, which we already checked above
	n, _ := p.ReadBuffer.Read(data)
	return data[:n]
}

// HasData returns true if there's data available to read
func (p *PipeState) HasData() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ReadBuffer.Len() > 0
}

// Transact performs a combined write+read operation (FSCTL_PIPE_TRANSCEIVE)
// This is the primary method used by Windows clients for RPC over named pipes
func (p *PipeState) Transact(inputData []byte, maxOutput int) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(inputData) < HeaderSize {
		return nil, nil // Ignore short writes
	}

	hdr, err := ParseHeader(inputData)
	if err != nil {
		return nil, err
	}

	var response []byte

	switch hdr.PacketType {
	case PDUBind:
		// Handle RPC bind
		bindReq, err := ParseBindRequest(inputData)
		if err != nil {
			return nil, err
		}

		response = p.Handler.HandleBind(bindReq)
		p.Bound = true

	case PDURequest:
		// Handle RPC request
		if !p.Bound {
			// Not bound yet, ignore
			return nil, nil
		}
		rpcReq, err := ParseRequest(inputData)
		if err != nil {
			return nil, err
		}
		response = p.Handler.HandleRequest(rpcReq)
	}

	// Limit response to maxOutput
	if len(response) > maxOutput && maxOutput > 0 {
		response = response[:maxOutput]
	}

	return response, nil
}

// =============================================================================
// Pipe Manager
// =============================================================================

// PipeManager manages named pipe instances
type PipeManager struct {
	mu        sync.RWMutex
	pipes     map[[16]byte]*PipeState // Keyed by SMB FileID
	shares    []ShareInfo1            // Available shares for enumeration
	sidMapper *sid.SIDMapper          // For lsarpc SID resolution
}

// NewPipeManager creates a new pipe manager
func NewPipeManager() *PipeManager {
	return &PipeManager{
		pipes:  make(map[[16]byte]*PipeState),
		shares: []ShareInfo1{},
	}
}

// SetShares updates the list of shares available for enumeration
func (pm *PipeManager) SetShares(shares []ShareInfo1) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.shares = shares
}

// SetSIDMapper sets the SID mapper used by lsarpc pipes for SID resolution.
func (pm *PipeManager) SetSIDMapper(mapper *sid.SIDMapper) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.sidMapper = mapper
}

// CreatePipe creates a new named pipe instance.
// The handler type is determined by the pipe name: "lsarpc" creates an
// LSARPCHandler, all others create an SRVSVCHandler.
func (pm *PipeManager) CreatePipe(fileID [16]byte, pipeName string) *PipeState {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var handler PipeHandler

	if isLSARPCPipe(pipeName) {
		// Create LSA handler for SID-to-name resolution
		mapper := pm.sidMapper
		if mapper == nil {
			mapper = sid.NewSIDMapper(0, 0, 0) // fallback
		}
		handler = NewLSARPCHandler(mapper)
	} else {
		// Create SRVSVC handler for share enumeration
		sharesCopy := make([]ShareInfo1, len(pm.shares))
		copy(sharesCopy, pm.shares)
		handler = NewSRVSVCHandler(sharesCopy)
	}

	pipe := NewPipeState(pipeName, handler)
	pm.pipes[fileID] = pipe

	return pipe
}

// GetPipe retrieves a pipe by its file ID
func (pm *PipeManager) GetPipe(fileID [16]byte) *PipeState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.pipes[fileID]
}

// ClosePipe closes a pipe
func (pm *PipeManager) ClosePipe(fileID [16]byte) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.pipes, fileID)
}

// IsSupportedPipe returns true if the pipe name is supported.
// Note: The caller is expected to normalize the pipe name to lowercase before calling.
func IsSupportedPipe(name string) bool {
	switch name {
	case "srvsvc", "\\srvsvc", "\\pipe\\srvsvc":
		return true
	case "lsarpc", "\\lsarpc", "\\pipe\\lsarpc":
		return true
	default:
		return false
	}
}

// isLSARPCPipe returns true if the pipe name refers to the lsarpc pipe.
func isLSARPCPipe(name string) bool {
	return strings.Contains(name, "lsarpc")
}
