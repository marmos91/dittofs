package session

// Credit-related constants
const (
	// DefaultInitialCredits is the base credit floor used when the client
	// requests 0 credits. Matches Samba's initial server grant
	// (source3/smbd/smb2_server.c:304 sets xconn->smb2.credits.granted = 1).
	// The server grows the client's credit pool in response to the client's
	// CreditRequest field on subsequent operations, bounded by the connection
	// window cap (see MaxSessionCredits).
	//
	// Previously set to 256 with an adaptive load boost to ~384 per response.
	// That combination overflowed the Samba client's per-connection
	// uint16_t cur_credits counter (MS-SMB2 3.3.1.2; Samba client check at
	// libcli/smb/smbXcli_base.c:4295–4298) during rapid SESSION_SETUP/LOGOFF
	// loops, producing NT_STATUS_INVALID_NETWORK_RESPONSE after ~85 iterations
	// — see issue #378.
	DefaultInitialCredits = 1

	// MinimumCreditGrant is the minimum credits to grant per response.
	// Always granting at least 1 credit prevents client deadlock.
	MinimumCreditGrant = 1

	// MaximumCreditGrant is the maximum credits to grant per response.
	// Limits memory exposure from a single client.
	MaximumCreditGrant = 8192

	// DefaultCreditPerOp is the default credit charge for simple operations.
	DefaultCreditPerOp = 1

	// CreditUnitSize is the size of one credit unit for I/O operations (64KB).
	CreditUnitSize = 65536
)

// CreditStrategy defines the credit grant strategy.
type CreditStrategy uint

const (
	// StrategyFixed always grants a fixed number of credits.
	// Simple but doesn't adapt to client behavior.
	StrategyFixed CreditStrategy = iota

	// StrategyEcho grants what the client requests (capped by config).
	// Maintains client's credit pool, prevents starvation.
	StrategyEcho

	// StrategyAdaptive adjusts based on server load and client behavior.
	// Production-ready strategy that balances throughput and protection.
	StrategyAdaptive
)

// String returns the string representation of the credit strategy.
func (s CreditStrategy) String() string {
	switch s {
	case StrategyFixed:
		return "fixed"
	case StrategyEcho:
		return "echo"
	case StrategyAdaptive:
		return "adaptive"
	default:
		return "unknown"
	}
}

// CreditConfig configures the credit management behavior.
type CreditConfig struct {
	// MinGrant is the minimum credits to grant per response.
	MinGrant uint16

	// MaxGrant is the maximum credits to grant per response.
	MaxGrant uint16

	// InitialGrant is the credits granted for initial requests (NEGOTIATE).
	InitialGrant uint16

	// MaxSessionCredits limits total outstanding credits per session.
	MaxSessionCredits uint32

	// LoadThresholdHigh triggers throttling when active requests exceed this.
	LoadThresholdHigh int64

	// LoadThresholdLow triggers credit boost when active requests are below this.
	LoadThresholdLow int64

	// AggressiveClientThreshold triggers throttling when a session has this many
	// outstanding requests.
	AggressiveClientThreshold int64
}

// DefaultCreditConfig returns a production-ready configuration aligned with
// Samba's server defaults (`smb2 max credits = 8192`, initial grant = 1).
// See applyDefaults in pkg/adapter/smb/config.go for the rationale (#378).
func DefaultCreditConfig() CreditConfig {
	return CreditConfig{
		MinGrant:                  1,
		MaxGrant:                  MaximumCreditGrant,
		InitialGrant:              DefaultInitialCredits,
		MaxSessionCredits:         8192,
		LoadThresholdHigh:         1000,
		LoadThresholdLow:          100,
		AggressiveClientThreshold: 256,
	}
}

// CalculateCreditCharge computes the credit charge for a READ/WRITE operation.
//
// For operations up to 64KB, the charge is 1 credit.
// For larger operations, the charge is ceiling(bytes / 65536).
//
// Example:
//
//	charge := CalculateCreditCharge(128 * 1024) // Returns 2 for 128KB
func CalculateCreditCharge(bytes uint32) uint16 {
	if bytes == 0 {
		return 1
	}
	// Ceiling division: (bytes + 65535) / 65536
	return uint16((uint64(bytes) + CreditUnitSize - 1) / CreditUnitSize)
}
