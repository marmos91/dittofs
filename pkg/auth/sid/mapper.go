package sid

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// SIDMapper provides Unix-to-Windows identity mapping using a stable machine SID.
//
// The mapper uses Samba-style RID allocation to prevent collisions between
// user and group SIDs: user RID = uid*2 + 1000, group RID = gid*2 + 1001.
// This guarantees UserSID(n) != GroupSID(n) for all n.
//
// The machine SID is of the form S-1-5-21-{a}-{b}-{c} where a, b, c are
// randomly generated 32-bit values persisted in the control plane store.
type SIDMapper struct {
	machineSID [3]uint32 // The three sub-authorities of the domain SID
}

// NewSIDMapper creates a mapper from explicit sub-authority values.
func NewSIDMapper(a, b, c uint32) *SIDMapper {
	return &SIDMapper{machineSID: [3]uint32{a, b, c}}
}

// NewSIDMapperFromString parses a machine SID string like "S-1-5-21-{a}-{b}-{c}"
// and creates a mapper.
func NewSIDMapperFromString(sidStr string) (*SIDMapper, error) {
	sid, err := ParseSIDString(sidStr)
	if err != nil {
		return nil, fmt.Errorf("invalid machine SID string: %w", err)
	}

	// Machine SID must be S-1-5-21-{a}-{b}-{c}
	if sid.Revision != 1 ||
		sid.IdentifierAuthority != [6]byte{0, 0, 0, 0, 0, 5} ||
		sid.SubAuthorityCount != 4 ||
		sid.SubAuthorities[0] != 21 {
		return nil, fmt.Errorf("machine SID must be S-1-5-21-{a}-{b}-{c}, got %s", sidStr)
	}

	return &SIDMapper{
		machineSID: [3]uint32{
			sid.SubAuthorities[1],
			sid.SubAuthorities[2],
			sid.SubAuthorities[3],
		},
	}, nil
}

// GenerateMachineSID creates a new mapper with randomly generated sub-authorities.
func GenerateMachineSID() *SIDMapper {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}

	return &SIDMapper{
		machineSID: [3]uint32{
			binary.LittleEndian.Uint32(buf[0:4]),
			binary.LittleEndian.Uint32(buf[4:8]),
			binary.LittleEndian.Uint32(buf[8:12]),
		},
	}
}

// MachineSIDString returns the machine SID as "S-1-5-21-{a}-{b}-{c}" for persistence.
func (m *SIDMapper) MachineSIDString() string {
	return fmt.Sprintf("S-1-5-21-%d-%d-%d",
		m.machineSID[0], m.machineSID[1], m.machineSID[2])
}

// UserSID returns the SID for a Unix UID.
//
// Special cases:
//   - UID 0 (root) maps to BUILTIN\Administrators (S-1-5-32-544)
//
// For all other UIDs, the RID is uid*2 + 1000, producing a SID of the form
// S-1-5-21-{a}-{b}-{c}-{RID}.
func (m *SIDMapper) UserSID(uid uint32) *SID {
	if uid == 0 {
		return WellKnownAdministrators
	}

	rid := uid*2 + 1000
	return m.makeDomainSID(rid)
}

// GroupSID returns the SID for a Unix GID.
//
// The RID is gid*2 + 1001, producing a SID of the form
// S-1-5-21-{a}-{b}-{c}-{RID}.
//
// The +1001 offset (vs +1000 for users) guarantees that
// UserSID(n) != GroupSID(n) for all n.
func (m *SIDMapper) GroupSID(gid uint32) *SID {
	rid := gid*2 + 1001
	return m.makeDomainSID(rid)
}

// UIDFromSID extracts the Unix UID from a domain user SID.
// Returns (uid, true) if the SID matches the machine domain and has an even
// RID offset (user pattern). Returns (0, false) otherwise.
func (m *SIDMapper) UIDFromSID(s *SID) (uint32, bool) {
	if !m.IsDomainSID(s) {
		return 0, false
	}

	rid := s.SubAuthorities[4]
	if rid < 1000 {
		return 0, false
	}

	offset := rid - 1000
	if offset%2 != 0 {
		return 0, false // Odd offset = group SID, not user
	}

	return offset / 2, true
}

// GIDFromSID extracts the Unix GID from a domain group SID.
// Returns (gid, true) if the SID matches the machine domain and has an odd
// RID offset (group pattern). Returns (0, false) otherwise.
func (m *SIDMapper) GIDFromSID(s *SID) (uint32, bool) {
	if !m.IsDomainSID(s) {
		return 0, false
	}

	rid := s.SubAuthorities[4]
	if rid < 1001 {
		return 0, false
	}

	offset := rid - 1001
	if offset%2 != 0 {
		return 0, false // Even offset = user SID, not group
	}

	return offset / 2, true
}

// IsDomainSID reports whether the SID belongs to this machine's domain.
// A domain SID has authority [0,0,0,0,0,5], 5 sub-authorities,
// first sub-authority == 21, and sub-authorities [1-3] matching the machine SID.
func (m *SIDMapper) IsDomainSID(s *SID) bool {
	return s != nil &&
		s.Revision == 1 &&
		s.IdentifierAuthority == [6]byte{0, 0, 0, 0, 0, 5} &&
		s.SubAuthorityCount == 5 &&
		s.SubAuthorities[0] == 21 &&
		s.SubAuthorities[1] == m.machineSID[0] &&
		s.SubAuthorities[2] == m.machineSID[1] &&
		s.SubAuthorities[3] == m.machineSID[2]
}

// PrincipalToSID converts an NFSv4 principal to a Windows SID.
//
// Mapping rules:
//   - "OWNER@": UserSID(fileOwnerUID)
//   - "GROUP@": GroupSID(fileOwnerGID)
//   - "EVERYONE@": WellKnownEveryone (S-1-1-0)
//   - "{uid}@domain": UserSID(uid) if uid is numeric
//   - Otherwise: hash-based UserSID as fallback
func (m *SIDMapper) PrincipalToSID(who string, fileOwnerUID, fileOwnerGID uint32) *SID {
	switch who {
	case acl.SpecialOwner:
		return m.UserSID(fileOwnerUID)
	case acl.SpecialGroup:
		return m.GroupSID(fileOwnerGID)
	case acl.SpecialEveryone:
		return WellKnownEveryone
	default:
		// Try to extract a numeric UID from "1000@localdomain" format
		if idx := strings.Index(who, "@"); idx > 0 {
			if uid, err := strconv.ParseUint(who[:idx], 10, 32); err == nil {
				return m.UserSID(uint32(uid))
			}
		}
		// Fallback: use a hash-based RID. Guard against empty/invalid
		// principals and avoid mapping to UID 0 / WellKnownAdministrators.
		var rid uint32
		for _, c := range who {
			rid = rid*31 + uint32(c)
		}
		if rid == 0 {
			rid = 1
		}
		// Generate a domain SID directly so we do not trigger any uid==0
		// special-cases that may exist in UserSID.
		return m.makeDomainSID(rid)
	}
}

// SIDToPrincipal converts a Windows SID to an NFSv4 principal string.
//
// Mapping rules:
//   - Well-known SIDs are mapped directly (S-1-1-0 -> "EVERYONE@")
//   - Domain user SIDs extract UID and format as "{uid}@localdomain"
//   - Domain group SIDs extract GID and format as "{gid}@localdomain"
//   - Unknown SIDs return their string representation
func (m *SIDMapper) SIDToPrincipal(s *SID) string {
	// Check well-known SIDs first
	sidStr := FormatSID(s)
	switch sidStr {
	case "S-1-1-0":
		return acl.SpecialEveryone
	case "S-1-3-0":
		return acl.SpecialOwner
	case "S-1-3-1":
		return acl.SpecialGroup
	}

	// Check domain user SIDs
	if uid, ok := m.UIDFromSID(s); ok {
		return fmt.Sprintf("%d@localdomain", uid)
	}

	// Check domain group SIDs
	if gid, ok := m.GIDFromSID(s); ok {
		return fmt.Sprintf("%d@localdomain", gid)
	}

	return sidStr
}

// makeDomainSID constructs a full domain SID with the machine sub-authorities and the given RID.
func (m *SIDMapper) makeDomainSID(rid uint32) *SID {
	return &SID{
		Revision:            1,
		SubAuthorityCount:   5,
		IdentifierAuthority: [6]byte{0, 0, 0, 0, 0, 5},
		SubAuthorities: []uint32{
			21,
			m.machineSID[0],
			m.machineSID[1],
			m.machineSID[2],
			rid,
		},
	}
}
