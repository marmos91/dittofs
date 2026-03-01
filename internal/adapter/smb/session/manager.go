package session

import (
	"sync"
	"sync/atomic"
	"time"
)

// Manager manages SMB2 sessions and provides unified credit tracking.
//
// It serves as the single source of truth for session lifecycle, replacing
// the previously separate Handler.sessions and CreditManager.sessions maps.
//
// Thread safety:
// All Manager methods are safe for concurrent use.
type Manager struct {
	// Session storage
	sessions      sync.Map // sessionID -> *Session
	nextSessionID atomic.Uint64

	// Credit configuration
	config   CreditConfig
	strategy CreditStrategy

	// Server-wide metrics for adaptive algorithm
	activeRequests  atomic.Int64  // Current outstanding requests across all sessions
	totalGrants     atomic.Uint64 // Cumulative credits granted
	totalOperations atomic.Uint64 // Total requests processed
}

// NewManager creates a new session metaSvc with the given credit configuration.
func NewManager(config CreditConfig) *Manager {
	return NewManagerWithStrategy(StrategyAdaptive, config)
}

// NewManagerWithStrategy creates a new session metaSvc with explicit strategy.
func NewManagerWithStrategy(strategy CreditStrategy, config CreditConfig) *Manager {
	m := &Manager{
		config:   config,
		strategy: strategy,
	}
	// Start session IDs at 1 (0 is reserved for pre-auth/anonymous)
	m.nextSessionID.Store(1)

	// Create the anonymous/pre-auth session (ID 0)
	// This handles credit tracking for NEGOTIATE and initial SESSION_SETUP
	anonymousSession := NewSession(0, "", false, "", "")
	m.sessions.Store(uint64(0), anonymousSession)

	return m
}

// NewDefaultManager creates a session metaSvc with adaptive strategy and default config.
func NewDefaultManager() *Manager {
	return NewManager(DefaultCreditConfig())
}

// =============================================================================
// Session Lifecycle
// =============================================================================

// CreateSession creates a new session and returns it.
// The session is automatically stored and ready for use.
func (m *Manager) CreateSession(clientAddr string, isGuest bool, username, domain string) *Session {
	sessionID := m.nextSessionID.Add(1)
	session := NewSession(sessionID, clientAddr, isGuest, username, domain)
	m.sessions.Store(sessionID, session)
	return session
}

// GetSession retrieves a session by ID.
// Returns nil and false if the session doesn't exist.
func (m *Manager) GetSession(sessionID uint64) (*Session, bool) {
	v, ok := m.sessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

// DeleteSession removes a session and cleans up all associated resources.
// This is the single point of session cleanup - no orphaned credit entries.
func (m *Manager) DeleteSession(sessionID uint64) {
	// Don't delete the anonymous session (ID 0)
	if sessionID == 0 {
		return
	}
	m.sessions.Delete(sessionID)
}

// StoreSession stores an externally created session.
// Use this for pending auth flows where the session ID is pre-generated.
// For normal session creation, use CreateSession instead.
func (m *Manager) StoreSession(session *Session) {
	m.sessions.Store(session.SessionID, session)
}

// GetOrCreateSession returns an existing session or creates a placeholder.
// Used for credit tracking on requests before authentication completes.
func (m *Manager) GetOrCreateSession(sessionID uint64) *Session {
	// Try to get existing session
	if session, ok := m.GetSession(sessionID); ok {
		return session
	}

	// For sessionID 0, return the anonymous session
	if sessionID == 0 {
		v, _ := m.sessions.Load(uint64(0))
		return v.(*Session)
	}

	// Create a temporary session for tracking
	// This handles the case where we receive requests for a session
	// that's being set up but not yet fully created
	session := NewSession(sessionID, "", false, "", "")
	actual, loaded := m.sessions.LoadOrStore(sessionID, session)
	if loaded {
		return actual.(*Session)
	}
	return session
}

// GenerateSessionID generates a new unique session ID.
// Used by handlers that need to reserve a session ID before CreateSession.
func (m *Manager) GenerateSessionID() uint64 {
	return m.nextSessionID.Add(1)
}

// =============================================================================
// Credit Operations
// =============================================================================

// RequestStarted records that a request has started processing.
// Should be called at the start of each request handler.
func (m *Manager) RequestStarted(sessionID uint64) {
	m.activeRequests.Add(1)
	if session, ok := m.GetSession(sessionID); ok {
		session.RequestStarted()
	}
}

// RequestCompleted records that a request has finished processing.
// Should be called when each request handler completes.
func (m *Manager) RequestCompleted(sessionID uint64) {
	m.activeRequests.Add(-1)
	if session, ok := m.GetSession(sessionID); ok {
		session.RequestCompleted()
	}
}

// GrantCredits calculates and records credits to grant in a response.
//
// Parameters:
//   - sessionID: The session making the request
//   - requested: Credits requested by the client
//   - creditCharge: Credits consumed by this operation
//
// Returns the number of credits to grant.
func (m *Manager) GrantCredits(sessionID uint64, requested uint16, creditCharge uint16) uint16 {
	session, ok := m.GetSession(sessionID)
	if !ok {
		// Session was deleted (e.g., after LOGOFF). Don't re-create it.
		// Grant a minimal credit so the client can send the next request.
		// For sessionID 0 (pre-auth), use the anonymous session.
		if sessionID == 0 {
			session = m.GetOrCreateSession(0)
		} else {
			return 1
		}
	}
	session.credits.LastActivity.Store(time.Now().Unix())

	// Record consumption
	session.ConsumeCredits(creditCharge)

	// Calculate grant based on strategy
	var grant uint16
	switch m.strategy {
	case StrategyFixed:
		grant = m.grantFixed()
	case StrategyEcho:
		grant = m.grantEcho(requested)
	case StrategyAdaptive:
		grant = m.grantAdaptive(session, requested)
	default:
		grant = m.grantFixed()
	}

	// Record grant
	session.GrantCredits(grant)

	// Update server-wide metrics
	m.totalGrants.Add(uint64(grant))
	m.totalOperations.Add(1)

	return grant
}

// grantFixed implements the fixed grant strategy.
func (m *Manager) grantFixed() uint16 {
	return m.config.InitialGrant
}

// grantEcho implements the echo grant strategy.
func (m *Manager) grantEcho(requested uint16) uint16 {
	if requested == 0 {
		return m.config.InitialGrant
	}
	if requested < m.config.MinGrant {
		return m.config.MinGrant
	}
	if requested > m.config.MaxGrant {
		return m.config.MaxGrant
	}
	return requested
}

// grantAdaptive implements the adaptive grant strategy.
func (m *Manager) grantAdaptive(session *Session, requested uint16) uint16 {
	// Base grant starts at initial grant
	baseGrant := float64(m.config.InitialGrant)

	// Factor 1: Server load
	// Reduce grants when server is under high load
	activeReqs := m.activeRequests.Load()
	if activeReqs > m.config.LoadThresholdHigh {
		// Linear reduction: at 2x threshold, grant 50%
		loadFactor := float64(m.config.LoadThresholdHigh) / float64(activeReqs)
		if loadFactor < 0.25 {
			loadFactor = 0.25 // Never reduce below 25%
		}
		baseGrant *= loadFactor
	} else if activeReqs < m.config.LoadThresholdLow {
		// Boost grants when server is lightly loaded
		baseGrant *= 1.5
	}

	// Factor 2: Client behavior
	// Throttle aggressive clients with many outstanding requests
	clientOutstanding := session.GetOutstandingRequests()
	if clientOutstanding > m.config.AggressiveClientThreshold {
		// Reduce grants for aggressive clients
		clientFactor := float64(m.config.AggressiveClientThreshold) / float64(clientOutstanding)
		if clientFactor < 0.5 {
			clientFactor = 0.5 // Never reduce below 50% for client factor
		}
		baseGrant *= clientFactor
	}

	// Factor 3: Session outstanding credits
	// Prevent sessions from accumulating too many credits
	currentOutstanding := session.GetOutstanding()
	if currentOutstanding > 0 && uint32(currentOutstanding) > m.config.MaxSessionCredits/2 {
		// Approaching session limit, reduce grants
		sessionFactor := float64(m.config.MaxSessionCredits) / float64(currentOutstanding*2)
		if sessionFactor < 0.5 {
			sessionFactor = 0.5
		}
		baseGrant *= sessionFactor
	}

	// Convert to uint16 with bounds
	grant := uint16(baseGrant)
	if grant < m.config.MinGrant {
		grant = m.config.MinGrant
	}
	if grant > m.config.MaxGrant {
		grant = m.config.MaxGrant
	}

	// Honor client's request if reasonable (echo behavior within adaptive)
	if requested > 0 && requested < grant {
		// Client is being modest, respect that
		grant = requested
		if grant < m.config.MinGrant {
			grant = m.config.MinGrant
		}
	}

	return grant
}

// =============================================================================
// Statistics
// =============================================================================

// ManagerStats contains server-wide session and credit statistics.
type ManagerStats struct {
	ActiveRequests  int64
	TotalGrants     uint64
	TotalOperations uint64
	SessionCount    int
}

// GetStats returns current server-wide statistics.
func (m *Manager) GetStats() ManagerStats {
	sessionCount := 0
	m.sessions.Range(func(_, _ any) bool {
		sessionCount++
		return true
	})

	return ManagerStats{
		ActiveRequests:  m.activeRequests.Load(),
		TotalGrants:     m.totalGrants.Load(),
		TotalOperations: m.totalOperations.Load(),
		SessionCount:    sessionCount,
	}
}

// GetSessionStats returns statistics for a specific session.
// Returns nil if the session doesn't exist.
func (m *Manager) GetSessionStats(sessionID uint64) *SessionStats {
	session, ok := m.GetSession(sessionID)
	if !ok {
		return nil
	}
	stats := session.GetStats()
	return &stats
}
