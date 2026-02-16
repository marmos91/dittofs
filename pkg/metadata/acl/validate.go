package acl

import (
	"errors"
	"fmt"
)

var (
	// ErrACETooMany is returned when an ACL exceeds MaxACECount.
	ErrACETooMany = errors.New("ACL exceeds maximum ACE count")

	// ErrACEInvalidType is returned for unrecognized ACE types.
	ErrACEInvalidType = errors.New("invalid ACE type")

	// ErrACEEmptyWho is returned when an ACE has an empty Who field.
	ErrACEEmptyWho = errors.New("ACE has empty Who field")

	// ErrACLNotCanonical is returned when ACEs are not in canonical order.
	ErrACLNotCanonical = errors.New("ACL is not in canonical order")
)

// ValidateACL validates an entire ACL, checking:
//  1. ACE count does not exceed MaxACECount (128)
//  2. Each individual ACE is valid (type, who)
//  3. ACEs are in strict Windows canonical order:
//     Bucket 1: Explicit DENY (no INHERITED_ACE, type == DENIED)
//     Bucket 2: Explicit ALLOW (no INHERITED_ACE, type == ALLOWED)
//     Bucket 3: Inherited DENY (INHERITED_ACE, type == DENIED)
//     Bucket 4: Inherited ALLOW (INHERITED_ACE, type == ALLOWED)
//     AUDIT/ALARM ACEs can appear anywhere.
func ValidateACL(a *ACL) error {
	if a == nil {
		return nil
	}

	if len(a.ACEs) > MaxACECount {
		return fmt.Errorf("%w: %d ACEs (maximum %d)", ErrACETooMany, len(a.ACEs), MaxACECount)
	}

	// Validate individual ACEs and check canonical ordering.
	lastBucket := 0

	for i := range a.ACEs {
		ace := &a.ACEs[i]

		if err := ValidateACE(ace); err != nil {
			return fmt.Errorf("ACE %d: %w", i, err)
		}

		// AUDIT and ALARM ACEs can appear anywhere; they don't affect
		// access decisions, so they are not subject to canonical ordering.
		if ace.Type == ACE4_SYSTEM_AUDIT_ACE_TYPE || ace.Type == ACE4_SYSTEM_ALARM_ACE_TYPE {
			continue
		}

		bucket := aceBucket(ace)
		if bucket < lastBucket {
			return fmt.Errorf("%w: ACE %d (bucket %d) appears after ACE in bucket %d",
				ErrACLNotCanonical, i, bucket, lastBucket)
		}
		lastBucket = bucket
	}

	return nil
}

// ValidateACE validates an individual ACE's fields.
func ValidateACE(ace *ACE) error {
	if ace.Type > ACE4_SYSTEM_ALARM_ACE_TYPE {
		return fmt.Errorf("%w: %d", ErrACEInvalidType, ace.Type)
	}
	if ace.Who == "" {
		return ErrACEEmptyWho
	}
	return nil
}

// aceBucket returns the canonical ordering bucket for an ACE.
// Bucket 1: Explicit DENY
// Bucket 2: Explicit ALLOW
// Bucket 3: Inherited DENY
// Bucket 4: Inherited ALLOW
func aceBucket(ace *ACE) int {
	inherited := ace.Flag&ACE4_INHERITED_ACE != 0

	// Base bucket: explicit DENY=1/ALLOW=2, inherited DENY=3/ALLOW=4
	base := 0
	if inherited {
		base = 2
	}

	switch ace.Type {
	case ACE4_ACCESS_DENIED_ACE_TYPE:
		return base + 1
	case ACE4_ACCESS_ALLOWED_ACE_TYPE:
		return base + 2
	default:
		// AUDIT/ALARM should not reach here; treat as bucket 0 (anywhere).
		return 0
	}
}
