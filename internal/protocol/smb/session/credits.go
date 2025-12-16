package session

// Credit-related constants
const (
	// DefaultInitialCredits is the default number of credits granted after NEGOTIATE.
	// This provides a good balance between client responsiveness and server protection.
	DefaultInitialCredits = 256

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

// DefaultCreditConfig returns a production-ready configuration.
func DefaultCreditConfig() CreditConfig {
	return CreditConfig{
		MinGrant:                  16,
		MaxGrant:                  MaximumCreditGrant,
		InitialGrant:              DefaultInitialCredits,
		MaxSessionCredits:         65535, // ~64K credits max per session
		LoadThresholdHigh:         1000,  // Start throttling at 1000 active requests
		LoadThresholdLow:          100,   // Boost credits below 100 active requests
		AggressiveClientThreshold: 256,   // Throttle if client has 256+ outstanding
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
