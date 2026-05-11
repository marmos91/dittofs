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

	// ErrACLNotCanonical is the documented sentinel for "ACL is not in
	// canonical (Windows-presentation) order". ValidateACL no longer
	// returns this error — it is retained for callers that wish to
	// classify ACLs for normalization/display purposes via aceBucket.
	ErrACLNotCanonical = errors.New("ACL is not in canonical order")
)

// ValidateACL validates an entire ACL, checking:
//  1. ACE count does not exceed MaxACECount (128)
//  2. Each individual ACE is valid (type, who)
//
// ACE ordering is NOT validated. Per MS-DTYP §2.4.5 the ACL layout is
// an unordered array of ACEs; canonical order (explicit DENY before
// explicit ALLOW, etc.) is a presentation convention (Windows ACL
// editor) and not a wire requirement. Samba and Windows both accept
// non-canonical DACLs on SET_INFO Security; smbtorture acls.DENY1
// explicitly relies on this (trailing DENY ACE that does not override
// granted permissions). Access evaluation walks the ACE array in
// stored order (RFC 7530 §6.2.1 / MS-DTYP §2.5.3.2), so non-canonical
// ACLs evaluate deterministically.
func ValidateACL(a *ACL) error {
	if a == nil {
		return nil
	}

	if len(a.ACEs) > MaxACECount {
		return fmt.Errorf("%w: %d ACEs (maximum %d)", ErrACETooMany, len(a.ACEs), MaxACECount)
	}

	for i := range a.ACEs {
		if err := ValidateACE(&a.ACEs[i]); err != nil {
			return fmt.Errorf("ACE %d: %w", i, err)
		}
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
