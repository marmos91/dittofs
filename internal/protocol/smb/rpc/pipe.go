// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// This file implements named pipe state management for RPC communication.
package rpc

import (
	"bytes"
	"sync"
)

// =============================================================================
// Named Pipe State
// =============================================================================

// PipeState represents the state of a named pipe connection
type PipeState struct {
	mu         sync.Mutex
	Name       string         // Pipe name (e.g., "srvsvc")
	Bound      bool           // Whether RPC bind has completed
	Handler    *SRVSVCHandler // RPC handler for this pipe
	ReadBuffer *bytes.Buffer  // Buffered response data for READ
}

// NewPipeState creates a new pipe state
func NewPipeState(name string, handler *SRVSVCHandler) *PipeState {
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
	mu     sync.RWMutex
	pipes  map[[16]byte]*PipeState // Keyed by SMB FileID
	shares []ShareInfo1            // Available shares for enumeration
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

// CreatePipe creates a new named pipe instance
func (pm *PipeManager) CreatePipe(fileID [16]byte, pipeName string) *PipeState {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Create a defensive copy of shares to prevent race conditions
	// if SetShares is called while this handler is active
	sharesCopy := make([]ShareInfo1, len(pm.shares))
	copy(sharesCopy, pm.shares)

	// Create handler with copied shares
	handler := NewSRVSVCHandler(sharesCopy)
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
	default:
		return false
	}
}
