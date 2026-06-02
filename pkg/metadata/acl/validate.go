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

	// ErrACLTooLarge is returned when an ACL's serialized size exceeds
	// MaxDACLSize.
	ErrACLTooLarge = errors.New("ACL exceeds maximum serialized size")

	// ErrACLNotCanonical is the documented sentinel for "ACL is not in
	// canonical (Windows-presentation) order". ValidateACL no longer
	// returns this error — it is retained for callers that wish to
	// classify ACLs for normalization/display purposes via aceBucket.
	ErrACLNotCanonical = errors.New("ACL is not in canonical order")
)

// aceWireOverhead is the fixed per-ACE NFSv4 wire footprint preceding the
// variable-length Who string: type(4) + flag(4) + access_mask(4) +
// who-length(4). The Who string itself (4-byte aligned) is added on top.
const aceWireOverhead = 16

// xdrPad rounds n up to the next 4-byte XDR boundary. NFSv4 opaque/string
// fields are zero-padded to a multiple of 4, so the on-wire footprint of a
// Who string is len rounded up — accounting for it makes the size estimate an
// accurate upper bound rather than an undershoot that could let a >64KB ACL
// pass (per Copilot review).
func xdrPad(n int) int { return (n + 3) &^ 3 }

// ValidateACL validates an entire ACL, checking:
//  1. ACE count does not exceed MaxACECount (128)
//  2. Estimated serialized size does not exceed MaxDACLSize (64KB)
//  3. Each individual ACE is valid (type, who)
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

	// Enforce MaxDACLSize. ACE.Who is an unbounded Go string, so the 128-ACE
	// count cap alone does not bound the serialized size — large Who strings
	// can blow past 64KB. The per-ACE footprint is the fixed header plus the
	// 4-byte-aligned Who string; the leading 4 bytes account for the ACE-array
	// length prefix so the estimate is an upper bound on the wire size.
	size := 4
	for i := range a.ACEs {
		size += aceWireOverhead + xdrPad(len(a.ACEs[i].Who))
		if size > MaxDACLSize {
			return fmt.Errorf("%w: exceeds %d bytes", ErrACLTooLarge, MaxDACLSize)
		}
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
