package handlers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FILE_FULL_EA_INFORMATION wire codec (MS-FSCC §2.4.15)
// ============================================================================
//
// A FILE_FULL_EA_INFORMATION chain encodes a file's extended attributes as a
// series of 4-byte-aligned entries:
//
//	+0  4B NextEntryOffset (LE) — bytes from the start of THIS entry to the
//	                              next; 0 ⇒ last entry.
//	+4  1B Flags
//	+5  1B EaNameLength          — length of EaName, NOT counting the NUL.
//	+6  2B EaValueLength (LE)
//	+8  N  EaName                — ASCII, followed by one mandatory NUL byte.
//	+8+N+1 M EaValue             — raw bytes.
//
// Entries are padded so the next entry starts on a 4-byte boundary.

// eaEntry is a decoded FILE_FULL_EA_INFORMATION entry.
type eaEntry struct {
	flags byte
	name  string
	value []byte
}

// decodeFullEaEntries parses a FILE_FULL_EA_INFORMATION chain into name/value/
// flags tuples. Returns an error if any entry is malformed (truncated header,
// name, value, or an invalid NextEntryOffset).
func decodeFullEaEntries(buffer []byte) ([]eaEntry, error) {
	var entries []eaEntry
	offset := 0
	for {
		if offset+8 > len(buffer) {
			if offset == len(buffer) {
				return entries, nil
			}
			return nil, fmt.Errorf("FILE_FULL_EA_INFORMATION: entry header at offset %d truncated", offset)
		}
		nextEntryOffset := uint32(buffer[offset]) |
			uint32(buffer[offset+1])<<8 |
			uint32(buffer[offset+2])<<16 |
			uint32(buffer[offset+3])<<24
		flags := buffer[offset+4]
		nameLen := int(buffer[offset+5])
		valueLen := int(uint16(buffer[offset+6]) | uint16(buffer[offset+7])<<8)
		nameStart := offset + 8
		nameEnd := nameStart + nameLen
		// +1 for the trailing NUL byte mandated by MS-FSCC §2.4.15.
		if nameEnd+1 > len(buffer) {
			return nil, fmt.Errorf("FILE_FULL_EA_INFORMATION: name at offset %d truncated", offset)
		}
		valueStart := nameEnd + 1
		valueEnd := valueStart + valueLen
		if valueEnd > len(buffer) {
			return nil, fmt.Errorf("FILE_FULL_EA_INFORMATION: value at offset %d truncated", offset)
		}

		value := make([]byte, valueLen)
		copy(value, buffer[valueStart:valueEnd])
		entries = append(entries, eaEntry{
			flags: flags,
			name:  string(buffer[nameStart:nameEnd]),
			value: value,
		})

		if nextEntryOffset == 0 {
			return entries, nil
		}
		newOffset := offset + int(nextEntryOffset)
		if newOffset <= offset || newOffset > len(buffer) {
			return nil, fmt.Errorf("FILE_FULL_EA_INFORMATION: invalid NextEntryOffset %d at %d", nextEntryOffset, offset)
		}
		offset = newOffset
	}
}

// eaMutationsFromEntries converts decoded SET_INFO EA entries into store
// EAMutations. Per MS-FSCC §2.4.15 and Samba (smbd ea set): an entry with a
// zero-length value DELETES the named EA; a non-empty value upserts it. The
// reserved ACL-xattr name is filtered by the caller before this runs.
func eaMutationsFromEntries(entries []eaEntry) []metadata.EAMutation {
	muts := make([]metadata.EAMutation, 0, len(entries))
	for _, e := range entries {
		if len(e.value) == 0 {
			muts = append(muts, metadata.EAMutation{Name: e.name, Delete: true})
			continue
		}
		muts = append(muts, metadata.EAMutation{Name: e.name, Value: e.value})
	}
	return muts
}

// encodeFullEaInformation serialises a file's extended attributes into a
// FILE_FULL_EA_INFORMATION chain (MS-FSCC §2.4.15). EAs are emitted in a
// stable, case-insensitive name order so the wire output is deterministic.
// The reserved ACL-xattr slot (security.NTACL) is never enumerated — it is
// the server's private NT-ACL store, mirroring Samba's vfs_acl_xattr.
//
// Returns an empty (non-nil) slice when the file has no enumerable EAs; the
// caller decides how to frame an empty list for the specific info class.
func encodeFullEaInformation(eas map[string][]byte) []byte {
	names := make([]string, 0, len(eas))
	for name := range eas {
		if isReservedACLXattrName(name) {
			continue
		}
		// EaNameLength is a uint8 and EaValueLength a uint16 on the wire
		// (MS-FSCC §2.4.15). Skip any entry that cannot be represented rather
		// than emit a length field that wraps and corrupts the chain. EAs
		// arriving via SMB are already bounded by these field widths; this only
		// guards values written through a non-SMB metadata path. Filtering here
		// (before the chain is built) keeps the last-entry / NextEntryOffset
		// bookkeeping below correct.
		if len(name) > 0xFF || len(eas[name]) > 0xFFFF {
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		li, lj := strings.ToUpper(names[i]), strings.ToUpper(names[j])
		if li == lj {
			return names[i] < names[j]
		}
		return li < lj
	})

	if len(names) == 0 {
		return []byte{}
	}

	var out []byte
	for idx, name := range names {
		value := eas[name]
		nameBytes := []byte(name)
		// Fixed header (8) + name + NUL (1) + value.
		entry := make([]byte, 8+len(nameBytes)+1+len(value))
		entry[4] = 0                    // Flags
		entry[5] = byte(len(nameBytes)) // EaNameLength (excludes NUL)
		entry[6] = byte(len(value))
		entry[7] = byte(len(value) >> 8) // EaValueLength (LE uint16)
		copy(entry[8:], nameBytes)
		// entry[8+len(nameBytes)] is the NUL terminator (already zero).
		copy(entry[8+len(nameBytes)+1:], value)

		// 4-byte align the entry so the next one starts on a boundary. The
		// last entry is NOT padded and keeps NextEntryOffset = 0.
		if idx < len(names)-1 {
			if pad := len(entry) % 4; pad != 0 {
				entry = append(entry, make([]byte, 4-pad)...)
			}
			nextOff := uint32(len(entry))
			entry[0] = byte(nextOff)
			entry[1] = byte(nextOff >> 8)
			entry[2] = byte(nextOff >> 16)
			entry[3] = byte(nextOff >> 24)
		}
		out = append(out, entry...)
	}
	return out
}

// fullEaInformationSize returns the number of bytes the EA chain for these EAs
// occupies on the wire (MS-FSCC §2.4.15), used to populate the EaSize field of
// FILE_ALL_INFORMATION / FILE_EA_INFORMATION. Matches encodeFullEaInformation's
// layout exactly (including inter-entry 4-byte padding).
func fullEaInformationSize(eas map[string][]byte) uint32 {
	encoded := encodeFullEaInformation(eas)
	return uint32(len(encoded))
}
