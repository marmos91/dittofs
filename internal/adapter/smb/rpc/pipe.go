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
	Name       string        // Pipe name (e.g., "srvsvc", "lsarpc")
	Bound      bool          // Whether RPC bind has completed
	Handler    PipeHandler   // RPC handler for this pipe
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

// dispatchRPC parses an RPC PDU and dispatches it to the appropriate handler.
// Returns the response bytes and any error. Must be called with p.mu held.
func (p *PipeState) dispatchRPC(data []byte) ([]byte, error) {
	if len(data) < HeaderSize {
		return nil, nil
	}

	hdr, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}

	switch hdr.PacketType {
	case PDUBind:
		bindReq, err := ParseBindRequest(data)
		if err != nil {
			return nil, err
		}
		response := p.Handler.HandleBind(bindReq)
		p.Bound = true
		return response, nil

	case PDUAlterContext:
		// Alter_Context shares the Bind body layout and elicits an
		// alter_context_resp that is byte-identical to a bind_ack except for the
		// PDU type. Windows' RPC runtime issues this to (re)negotiate a
		// presentation context / transfer syntax after the initial bind; dropping
		// it silently starves the client's follow-up READ (#1607).
		altReq, err := ParseAlterContext(data)
		if err != nil {
			return nil, err
		}
		response := p.Handler.HandleBind(altReq)
		if len(response) > 2 {
			response[2] = PDUAlterContextR // bind_ack (12) -> alter_context_resp (15)
		}
		p.Bound = true
		return response, nil

	case PDURequest:
		rpcReq, err := ParseRequest(data)
		if err != nil {
			return nil, err
		}
		if !p.Bound {
			// A request before a successful bind cannot be dispatched. Return a
			// DCE/RPC fault (rather than nothing) so the client's paired READ
			// always completes instead of parking forever (#1607).
			return EncodeFaultPDU(rpcReq.Header.CallID, NcaSProtoError), nil
		}
		return p.Handler.HandleRequest(rpcReq), nil

	default:
		return nil, nil
	}
}

// ProcessWrite handles a WRITE to the named pipe (client -> server)
// Returns data to be made available for subsequent READ
func (p *PipeState) ProcessWrite(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// A single named-pipe WRITE may carry more than one complete DCE/RPC PDU
	// (e.g. a client that coalesces BIND+request, or OpenPolicy+LookupSids, into
	// one write). Dispatch every complete PDU and enqueue each response, so no
	// PDU — and therefore no client READ waiting on its response — is silently
	// dropped (#1607).
	for len(data) >= HeaderSize {
		hdr, err := ParseHeader(data)
		if err != nil {
			return err
		}
		// Trailing bytes that are not a DCE/RPC v5 PDU (e.g. write padding) are
		// not misparsed as a bogus request — stop rather than fault the write.
		if hdr.VersionMajor != 5 {
			break
		}
		// Advance by the declared fragment length. Guard a malformed/zero
		// FragLength (which would not advance the loop) and a fragment that
		// claims more bytes than we received — there is no cross-write
		// reassembly, so treat the remainder as this final PDU.
		fragLen := int(hdr.FragLength)
		if fragLen < HeaderSize || fragLen > len(data) {
			fragLen = len(data)
		}

		response, err := p.dispatchRPC(data[:fragLen])
		if err != nil {
			return err
		}
		if len(response) > 0 {
			p.ReadBuffer.Write(response)
		}

		data = data[fragLen:]
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

	response, err := p.dispatchRPC(inputData)
	if err != nil {
		return nil, err
	}

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
	mu                sync.RWMutex
	pipes             map[[16]byte]*PipeState // Keyed by SMB FileID
	shares            []ShareInfo1            // Available shares for enumeration
	sidMapper         *sid.SIDMapper          // For lsarpc SID resolution
	identityResolver  IdentityResolver        // For lsarpc real name resolution
	foreignResolver   ForeignSIDResolver      // For lsarpc AD/LDAP SID resolution
	machineDomainName string                  // NetBIOS name for machine-domain SIDs
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

// SetIdentityResolver sets the resolver for real username/group name lookup.
func (pm *PipeManager) SetIdentityResolver(resolver IdentityResolver) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.identityResolver = resolver
}

// SetForeignSIDResolver sets the directory-backed resolver used by lsarpc to
// translate foreign (AD/LDAP) domain SIDs to account names. May be nil.
func (pm *PipeManager) SetForeignSIDResolver(resolver ForeignSIDResolver) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.foreignResolver = resolver
}

// SetMachineDomainName sets the NetBIOS domain name reported by lsarpc for
// machine-domain (local UID/GID) SIDs. An empty value uses the default.
func (pm *PipeManager) SetMachineDomainName(name string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.machineDomainName = name
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
		lsaHandler := NewLSARPCHandler(mapper, pm.identityResolver)
		lsaHandler.SetForeignSIDResolver(pm.foreignResolver)
		lsaHandler.SetMachineDomainName(pm.machineDomainName)
		handler = lsaHandler
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
