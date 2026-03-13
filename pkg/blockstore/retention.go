package blockstore

import (
	"fmt"
	"strings"
	"time"
)

// RetentionPolicy controls how cached blocks are retained on local storage.
// String-typed for GORM compatibility and JSON serialization.
type RetentionPolicy string

const (
	// RetentionPin keeps blocks cached indefinitely (no eviction).
	RetentionPin RetentionPolicy = "pin"

	// RetentionTTL evicts blocks after a configurable time-to-live.
	RetentionTTL RetentionPolicy = "ttl"

	// RetentionLRU evicts least-recently-used blocks when space is needed.
	// This is the default policy for backward compatibility (CACHE-06).
	RetentionLRU RetentionPolicy = "lru"
)

// String returns the string representation of the retention policy.
func (p RetentionPolicy) String() string {
	return string(p)
}

// IsValid returns true if the retention policy is a recognized value.
func (p RetentionPolicy) IsValid() bool {
	switch p {
	case RetentionPin, RetentionTTL, RetentionLRU:
		return true
	default:
		return false
	}
}

// ParseRetentionPolicy parses a string into a RetentionPolicy.
// Empty or blank input defaults to LRU for backward compatibility (CACHE-06).
// Parsing is case-insensitive.
func ParseRetentionPolicy(s string) (RetentionPolicy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return RetentionLRU, nil
	}

	p := RetentionPolicy(strings.ToLower(s))
	if !p.IsValid() {
		return "", fmt.Errorf("invalid retention policy %q: must be one of pin, ttl, lru", s)
	}
	return p, nil
}

// ValidateRetentionPolicy checks that the policy and TTL combination is valid.
// TTL mode requires a positive duration. Pin and LRU modes accept any TTL value.
func ValidateRetentionPolicy(policy RetentionPolicy, ttl time.Duration) error {
	if !policy.IsValid() {
		return fmt.Errorf("invalid retention policy %q", policy)
	}
	if policy == RetentionTTL && ttl <= 0 {
		return fmt.Errorf("retention policy %q requires a positive TTL duration", policy)
	}
	return nil
}
