package portmap

import (
	"github.com/marmos91/dittofs/internal/protocol/portmap/handlers"
	"github.com/marmos91/dittofs/internal/protocol/portmap/types"
)

// HandlerResult contains the response data from a portmap procedure handler.
type HandlerResult struct {
	// Data contains the XDR-encoded response to send to the client.
	Data []byte
}

// PmapProcedureHandler defines the signature for portmap procedure handlers.
// It takes the handler (for registry access), the procedure argument data,
// and the client address string for access control (SET/UNSET localhost restriction).
type PmapProcedureHandler func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error)

// PmapProcedure contains metadata about a portmap procedure for dispatch.
type PmapProcedure struct {
	// Name is the procedure name for logging (e.g., "NULL", "GETPORT").
	Name string

	// Handler is the function that processes this procedure.
	Handler PmapProcedureHandler
}

// DispatchTable maps portmap procedure numbers to their handlers.
//
// Procedure 5 (CALLIT) is intentionally omitted to prevent DDoS amplification
// attacks. CALLIT forwards RPC calls to other programs, which allows attackers
// to use the portmapper as an amplifier. Modern rpcbind implementations also
// disable or restrict CALLIT.
var DispatchTable map[uint32]*PmapProcedure

func init() {
	DispatchTable = map[uint32]*PmapProcedure{
		types.ProcNull: {
			Name: "NULL",
			Handler: func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error) {
				return &HandlerResult{Data: handler.Null()}, nil
			},
		},
		types.ProcSet: {
			Name: "SET",
			Handler: func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error) {
				resp, err := handler.Set(data, clientAddr)
				return &HandlerResult{Data: resp}, err
			},
		},
		types.ProcUnset: {
			Name: "UNSET",
			Handler: func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error) {
				resp, err := handler.Unset(data, clientAddr)
				return &HandlerResult{Data: resp}, err
			},
		},
		types.ProcGetport: {
			Name: "GETPORT",
			Handler: func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error) {
				resp, err := handler.Getport(data)
				return &HandlerResult{Data: resp}, err
			},
		},
		types.ProcDump: {
			Name: "DUMP",
			Handler: func(handler *handlers.Handler, data []byte, clientAddr string) (*HandlerResult, error) {
				return &HandlerResult{Data: handler.Dump()}, nil
			},
		},
	}
}
