package state

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// Session represents an NFSv4.1 session per RFC 8881 Section 2.10.
//
// A session ties together a session ID, client ID, fore/back channel
// slot tables, and negotiated channel attributes. Sessions are created
// by the CREATE_SESSION operation (Phase 19) and looked up by ID in
// the SEQUENCE operation (Phase 20).
//
// This struct is intentionally independent of StateManager -- the
// CREATE_SESSION handler creates a Session via NewSession and then
// registers it with the manager separately.
type Session struct {
	// SessionID is the unique 16-byte session identifier (crypto/rand generated).
	SessionID types.SessionId4

	// ClientID is the server-assigned client ID that owns this session.
	ClientID uint64

	// ForeChannelSlots is the slot table for fore channel (client -> server).
	ForeChannelSlots *SlotTable

	// BackChannelSlots is the slot table for back channel (server -> client).
	// nil if no back channel was requested.
	BackChannelSlots *SlotTable

	// ForeChannelAttrs holds the negotiated fore channel attributes.
	ForeChannelAttrs types.ChannelAttrs

	// BackChannelAttrs holds the negotiated back channel attributes.
	BackChannelAttrs types.ChannelAttrs

	// Flags holds the CREATE_SESSION flags (e.g., CREATE_SESSION4_FLAG_CONN_BACK_CHAN).
	Flags uint32

	// CbProgram is the callback RPC program number from CREATE_SESSION.
	CbProgram uint32

	// CreatedAt is when this session was created.
	CreatedAt time.Time

	// ============================================================================
	// Backchannel State (Phase 22)
	// ============================================================================

	// BackchannelSecParms stores the callback security parameters from
	// BACKCHANNEL_CTL. Updated when the client sends BACKCHANNEL_CTL.
	BackchannelSecParms []types.CallbackSecParms4

	// backchannelSender is the goroutine that sends callbacks over back-bound
	// connections. nil until first backchannel operation or first back-channel bind.
	backchannelSender *BackchannelSender
}

// NewSession creates a new Session with a crypto/rand-generated session ID,
// fore channel slot table, and optionally a back channel slot table if the
// CREATE_SESSION4_FLAG_CONN_BACK_CHAN flag is set.
//
// The fore channel slot table is always created from foreAttrs.MaxRequests.
// The back channel slot table is only created when flags includes
// CREATE_SESSION4_FLAG_CONN_BACK_CHAN.
//
// NewSlotTable clamps the slot count to [MinSlots, DefaultMaxSlots].
//
// This constructor does NOT register the session with StateManager.
// Registration is Phase 19's responsibility (CREATE_SESSION handler).
func NewSession(clientID uint64, foreAttrs, backAttrs types.ChannelAttrs, flags, cbProgram uint32) (*Session, error) {
	var sid types.SessionId4

	// Generate a random 16-byte session ID using crypto/rand.
	// Session IDs are protocol-visible identifiers; predictable values could
	// allow session hijacking, so we fail rather than fall back to a weak source.
	if _, err := rand.Read(sid[:]); err != nil {
		return nil, fmt.Errorf("failed to generate session ID: %w", err)
	}

	s := &Session{
		SessionID:        sid,
		ClientID:         clientID,
		ForeChannelAttrs: foreAttrs,
		BackChannelAttrs: backAttrs,
		Flags:            flags,
		CbProgram:        cbProgram,
		CreatedAt:        time.Now(),
	}

	// Always create fore channel slot table.
	s.ForeChannelSlots = NewSlotTable(foreAttrs.MaxRequests)

	// Create back channel slot table only if CONN_BACK_CHAN flag is set.
	if flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN != 0 {
		s.BackChannelSlots = NewSlotTable(backAttrs.MaxRequests)
	}

	return s, nil
}

// HasInFlightRequests returns true if the session's fore channel slot table
// has any slots currently in use (processing a request).
func (s *Session) HasInFlightRequests() bool {
	if s.ForeChannelSlots == nil {
		return false
	}
	return s.ForeChannelSlots.HasInFlightRequests()
}
