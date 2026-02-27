// Package sid provides Windows Security Identifier (SID) types, encoding,
// decoding, and mapping for cross-protocol identity interoperability.
//
// SIDs are binary identifiers used by Windows to represent security principals
// (users, groups, computers). This package enables both SMB and NFS adapters
// to produce consistent SIDs for the same Unix identities.
//
// The binary format follows MS-DTYP Section 2.4.2:
//
//	Revision(1) + SubAuthorityCount(1) + IdentifierAuthority(6, big-endian)
//	+ SubAuthorities(4*N, little-endian)
//
// The string format is "S-{Revision}-{Authority}-{SubAuth1}-...-{SubAuthN}".
package sid

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// SID represents a Windows Security Identifier per MS-DTYP Section 2.4.2.
//
// A SID uniquely identifies a security principal (user, group, or computer).
// The binary encoding is: Revision(1) + SubAuthorityCount(1) +
// IdentifierAuthority(6, big-endian) + SubAuthorities(4*N, little-endian).
type SID struct {
	// Revision is always 1.
	Revision uint8

	// SubAuthorityCount is the number of sub-authority values.
	SubAuthorityCount uint8

	// IdentifierAuthority is the top-level authority (6 bytes, big-endian).
	IdentifierAuthority [6]byte

	// SubAuthorities contains the sub-authority values.
	SubAuthorities []uint32
}

// SIDSize returns the binary size of a SID in bytes.
func SIDSize(sid *SID) int {
	return 8 + 4*int(sid.SubAuthorityCount)
}

// EncodeSID writes a binary SID to buf per MS-DTYP Section 2.4.2.
func EncodeSID(buf *bytes.Buffer, sid *SID) {
	buf.WriteByte(sid.Revision)
	buf.WriteByte(sid.SubAuthorityCount)
	buf.Write(sid.IdentifierAuthority[:])
	for _, sa := range sid.SubAuthorities {
		_ = binary.Write(buf, binary.LittleEndian, sa)
	}
}

// DecodeSID parses a binary SID from data per MS-DTYP Section 2.4.2.
// Returns the parsed SID and number of bytes consumed, or an error.
func DecodeSID(data []byte) (*SID, int, error) {
	if len(data) < 8 {
		return nil, 0, fmt.Errorf("SID too short: %d bytes", len(data))
	}

	sid := &SID{
		Revision:          data[0],
		SubAuthorityCount: data[1],
	}
	copy(sid.IdentifierAuthority[:], data[2:8])

	size := 8 + 4*int(sid.SubAuthorityCount)
	if len(data) < size {
		return nil, 0, fmt.Errorf("SID data too short for %d sub-authorities: have %d, need %d", sid.SubAuthorityCount, len(data), size)
	}

	sid.SubAuthorities = make([]uint32, sid.SubAuthorityCount)
	for i := 0; i < int(sid.SubAuthorityCount); i++ {
		offset := 8 + 4*i
		sid.SubAuthorities[i] = binary.LittleEndian.Uint32(data[offset : offset+4])
	}

	return sid, size, nil
}

// FormatSID formats a SID as a string in "S-1-5-21-..." format.
func FormatSID(sid *SID) string {
	// Compute the 48-bit authority value from big-endian 6 bytes
	var authority uint64
	for i := range 6 {
		authority = (authority << 8) | uint64(sid.IdentifierAuthority[i])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "S-%d-%d", sid.Revision, authority)
	for _, sa := range sid.SubAuthorities {
		fmt.Fprintf(&b, "-%d", sa)
	}
	return b.String()
}

// ParseSIDString parses a SID string in "S-1-5-21-..." format.
func ParseSIDString(s string) (*SID, error) {
	if !strings.HasPrefix(s, "S-") {
		return nil, fmt.Errorf("invalid SID format: must start with S-")
	}

	parts := strings.Split(s[2:], "-")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid SID format: need at least revision and authority")
	}

	revision, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return nil, fmt.Errorf("invalid SID revision: %w", err)
	}

	authority, err := strconv.ParseUint(parts[1], 10, 48)
	if err != nil {
		return nil, fmt.Errorf("invalid SID authority: %w", err)
	}

	sid := &SID{
		Revision:          uint8(revision),
		SubAuthorityCount: uint8(len(parts) - 2),
	}

	// Encode authority as big-endian 6 bytes
	for i := 5; i >= 0; i-- {
		sid.IdentifierAuthority[i] = byte(authority & 0xFF)
		authority >>= 8
	}

	sid.SubAuthorities = make([]uint32, sid.SubAuthorityCount)
	for i := 0; i < int(sid.SubAuthorityCount); i++ {
		val, err := strconv.ParseUint(parts[i+2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid SID sub-authority %d: %w", i, err)
		}
		sid.SubAuthorities[i] = uint32(val)
	}

	return sid, nil
}

// ParseSIDMust parses a SID string and panics on error. Used for well-known SIDs.
func ParseSIDMust(s string) *SID {
	sid, err := ParseSIDString(s)
	if err != nil {
		panic(fmt.Sprintf("invalid well-known SID %q: %v", s, err))
	}
	return sid
}

// Equal reports whether two SIDs are identical.
func (s *SID) Equal(other *SID) bool {
	if s == other {
		return true
	}
	if s == nil || other == nil {
		return false
	}
	if s.Revision != other.Revision {
		return false
	}
	if s.SubAuthorityCount != other.SubAuthorityCount {
		return false
	}
	if s.IdentifierAuthority != other.IdentifierAuthority {
		return false
	}
	if len(s.SubAuthorities) != len(other.SubAuthorities) {
		return false
	}
	for i := range s.SubAuthorities {
		if s.SubAuthorities[i] != other.SubAuthorities[i] {
			return false
		}
	}
	return true
}
