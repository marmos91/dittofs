// Package session provides SMB2 session and credit management.
//
// # Overview
//
// This package manages the lifecycle of SMB2 sessions and implements
// credit-based flow control for the SMB2 protocol.
//
// # Session Management
//
// Sessions represent authenticated connections from SMB clients:
//
//   - Created during SESSION_SETUP after successful authentication
//   - Track username, client address, and creation time
//   - Support both authenticated and guest sessions
//   - Destroyed on LOGOFF or connection close
//
// # Credit System
//
// SMB2 uses credits for flow control:
//
//   - Each request consumes credits (CreditCharge field)
//   - Responses grant credits (Credits field)
//   - Large operations consume more credits
//   - Prevents aggressive clients from overwhelming the server
//
// # Credit Strategies
//
// Three strategies control credit allocation:
//
//   - StrategyFixed: Always grant InitialGrant credits
//   - StrategyEcho: Grant what client requests (within bounds)
//   - StrategyAdaptive: Adjust based on server load (recommended)
//
// # Adaptive Strategy
//
// The adaptive strategy dynamically adjusts credits:
//
//   - Low load (< LoadThresholdLow): Grant maximum credits
//   - Normal load: Grant requested amount
//   - High load (> LoadThresholdHigh): Throttle to minimum
//   - Aggressive client: Throttle that specific session
//
// # Thread Safety
//
// All operations are thread-safe:
//
//   - Session storage uses sync.Map
//   - Credit counters use atomic operations
//   - Load tracking uses atomic counters
//
// # Configuration
//
// Credit configuration is provided via CreditConfig:
//
//	config := CreditConfig{
//	    MinGrant:          16,     // Minimum per response
//	    MaxGrant:          8192,   // Maximum per response
//	    InitialGrant:      256,    // For NEGOTIATE/SESSION_SETUP
//	    MaxSessionCredits: 65535,  // Total per session
//	}
//
// # Usage
//
//	// Create manager with configuration
//	manager := NewManager(config, StrategyAdaptive)
//
//	// Create session after authentication
//	session := manager.CreateSession(clientAddr, isGuest, username, domain)
//
//	// Track credits for a request
//	grant := manager.CalculateGrant(sessionID, creditRequest, creditCharge)
//
//	// Clean up
//	manager.DeleteSession(sessionID)
//
// # References
//
//   - [MS-SMB2] Section 3.3.1.2 - Per Session
//   - [MS-SMB2] Section 3.3.1.1 - Global
package session
