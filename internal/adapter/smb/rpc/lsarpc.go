// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// This file implements the LSA (Local Security Authority) RPC interface
// for SID-to-name resolution. Windows Explorer uses this to display
// human-readable names in the Security tab (Properties -> Security).
//
// Reference: [MS-LSAT] Local Security Authority (Translation Methods) Remote Protocol
// Reference: [MS-LSAD] Local Security Authority (Domain Policy) Remote Protocol
package rpc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// =============================================================================
// LSA Constants
// =============================================================================

// LSA interface UUID: 12345778-1234-abcd-ef00-0123456789ab
// This is the standard MS-LSAT interface UUID.
var LSAInterfaceUUID = [16]byte{
	0x78, 0x57, 0x34, 0x12, // 12345778
	0x34, 0x12, // 1234
	0xcd, 0xab, // abcd
	0xef, 0x00, // ef00
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, // 0123456789ab
}

// LSA Operation Numbers [MS-LSAT/MS-LSAD]
const (
	OpLsarClose       uint16 = 0  // LsarClose
	OpLsarOpenPolicy2 uint16 = 44 // LsarOpenPolicy2
	OpLsarLookupSids2 uint16 = 57 // LsarLookupSids2
	OpLsarLookupSids3 uint16 = 76 // LsarLookupSids3
)

// SID Name Types [MS-LSAT Section 2.2.13]
const (
	SidTypeUser           uint16 = 1
	SidTypeGroup          uint16 = 2
	SidTypeDomain         uint16 = 3
	SidTypeAlias          uint16 = 4
	SidTypeWellKnownGroup uint16 = 5
	SidTypeUnknown        uint16 = 8
)

// NT STATUS codes
const (
	statusSuccess       uint32 = 0x00000000
	statusSomeNotMapped uint32 = 0x00000107
	statusNotImpl       uint32 = 0xC0000002
)

// =============================================================================
// LSA Handler
// =============================================================================

// LSARPCHandler handles LSA RPC calls for SID-to-name resolution.
type LSARPCHandler struct {
	sidMapper *sid.SIDMapper
}

// NewLSARPCHandler creates a new LSA RPC handler with the given SID mapper.
func NewLSARPCHandler(sidMapper *sid.SIDMapper) *LSARPCHandler {
	return &LSARPCHandler{sidMapper: sidMapper}
}

// HandleBind processes a BIND request and returns a BIND_ACK for the LSA interface.
func (h *LSARPCHandler) HandleBind(req *BindRequest) []byte {
	transferSyntax := SyntaxID{UUID: NDRTransferSyntaxUUID, Version: 0x00000002}

	if len(req.ContextList) > 0 && len(req.ContextList[0].TransferSyntaxes) > 0 {
		transferSyntax = req.ContextList[0].TransferSyntaxes[0]
	}

	ack := &BindAck{
		MaxXmitFrag:  req.MaxXmitFrag,
		MaxRecvFrag:  req.MaxRecvFrag,
		AssocGroupID: 0x12345678,
		SecAddr:      "\\PIPE\\lsarpc",
		NumResults:   1,
		Results: []ContextResult{
			{
				Result:         0, // Acceptance
				Reason:         0,
				TransferSyntax: transferSyntax,
			},
		},
	}

	return ack.Encode(req.Header.CallID)
}

// HandleRequest processes an LSA RPC request and returns a response.
func (h *LSARPCHandler) HandleRequest(req *Request) []byte {
	logger.Debug("LSA RPC request", "opnum", req.OpNum)

	switch req.OpNum {
	case OpLsarClose:
		return h.handleClose(req)
	case OpLsarOpenPolicy2:
		return h.handleOpenPolicy2(req)
	case OpLsarLookupSids2:
		return h.handleLookupSids(req)
	case OpLsarLookupSids3:
		return h.handleLookupSids(req) // Same logic, no policy handle needed
	default:
		return h.buildFault(req.Header.CallID, 0x1C010003) // nca_op_rng_error
	}
}

// handleClose handles LsarClose (opnum 0).
// Returns success with a zeroed handle.
func (h *LSARPCHandler) handleClose(req *Request) []byte {
	// Response: zeroed policy handle (20 bytes) + status (4 bytes)
	stubData := make([]byte, 24)
	// Policy handle: 20 bytes of zeros (closed)
	// Status: 0 (success) at offset 20
	binary.LittleEndian.PutUint32(stubData[20:24], statusSuccess)

	resp := &Response{
		AllocHint:   uint32(len(stubData)),
		ContextID:   req.ContextID,
		CancelCount: 0,
		StubData:    stubData,
	}

	return resp.Encode(req.Header.CallID)
}

// handleOpenPolicy2 handles LsarOpenPolicy2 (opnum 44).
// Returns a stub policy handle (required before LookupSids2).
func (h *LSARPCHandler) handleOpenPolicy2(req *Request) []byte {
	// Response: policy handle (20 bytes) + status (4 bytes)
	stubData := make([]byte, 24)
	// Set a non-zero policy handle (fixed value)
	stubData[0] = 0x01
	stubData[4] = 0x02
	// Status: 0 (success)
	binary.LittleEndian.PutUint32(stubData[20:24], statusSuccess)

	resp := &Response{
		AllocHint:   uint32(len(stubData)),
		ContextID:   req.ContextID,
		CancelCount: 0,
		StubData:    stubData,
	}

	return resp.Encode(req.Header.CallID)
}

// resolvedSID contains a resolved SID with its display name and type.
type resolvedSID struct {
	name       string
	sidType    uint16
	domainName string
	domainSID  *sid.SID
}

// resolveSID resolves a single SID to a display name and type.
func (h *LSARPCHandler) resolveSID(s *sid.SID) resolvedSID {
	// Check well-known SIDs
	if name, ok := sid.WellKnownName(s); ok {
		// Determine SID type and domain from well-known name
		sidType := SidTypeWellKnownGroup
		var domainName string
		var domainSID *sid.SID

		if strings.HasPrefix(name, "NT AUTHORITY\\") {
			domainName = "NT AUTHORITY"
			domainSID = sid.ParseSIDMust("S-1-5")
			localName := strings.TrimPrefix(name, "NT AUTHORITY\\")
			return resolvedSID{name: localName, sidType: sidType, domainName: domainName, domainSID: domainSID}
		}
		if strings.HasPrefix(name, "BUILTIN\\") {
			domainName = "BUILTIN"
			domainSID = sid.ParseSIDMust("S-1-5-32")
			sidType = SidTypeAlias
			localName := strings.TrimPrefix(name, "BUILTIN\\")
			return resolvedSID{name: localName, sidType: sidType, domainName: domainName, domainSID: domainSID}
		}
		// No domain prefix (e.g., "Everyone")
		return resolvedSID{name: name, sidType: sidType, domainName: "", domainSID: nil}
	}

	// Check machine domain SIDs
	if uid, ok := h.sidMapper.UIDFromSID(s); ok {
		name := fmt.Sprintf("unix_user:%d", uid)
		return resolvedSID{
			name:       name,
			sidType:    SidTypeUser,
			domainName: "UNIX",
			domainSID:  h.machineDomainSID(),
		}
	}

	if gid, ok := h.sidMapper.GIDFromSID(s); ok {
		name := fmt.Sprintf("unix_group:%d", gid)
		return resolvedSID{
			name:       name,
			sidType:    SidTypeGroup,
			domainName: "UNIX",
			domainSID:  h.machineDomainSID(),
		}
	}

	// Unknown SID
	return resolvedSID{
		name:    sid.FormatSID(s),
		sidType: SidTypeUnknown,
	}
}

// machineDomainSID returns the machine domain SID (S-1-5-21-{a}-{b}-{c}) without the RID.
func (h *LSARPCHandler) machineDomainSID() *sid.SID {
	// Use a dummy UID to get a domain SID, then strip the RID
	fullSID := h.sidMapper.UserSID(1)
	if fullSID.SubAuthorityCount >= 5 {
		return &sid.SID{
			Revision:            fullSID.Revision,
			SubAuthorityCount:   4,
			IdentifierAuthority: fullSID.IdentifierAuthority,
			SubAuthorities:      fullSID.SubAuthorities[:4],
		}
	}
	return fullSID
}

// handleLookupSids handles LsarLookupSids2 (opnum 57) and LsarLookupSids3 (opnum 76).
// Parses the SID array, resolves each SID, and returns a response with
// referenced domains and translated names.
func (h *LSARPCHandler) handleLookupSids(req *Request) []byte {
	// Parse SIDs from the request stub data.
	// The request format varies between opnum 57 (has policy handle) and 76 (no handle).
	// For simplicity, we extract SIDs from the binary data.
	sids := h.parseSIDsFromRequest(req.StubData, req.OpNum)

	logger.Debug("LSA LookupSids", "numSids", len(sids))

	// Resolve each SID
	resolved := make([]resolvedSID, len(sids))
	allMapped := true
	for i, s := range sids {
		resolved[i] = h.resolveSID(s)
		if resolved[i].sidType == SidTypeUnknown {
			allMapped = false
		}
	}

	// Build domain list (deduplicated)
	domainMap := make(map[string]int) // domain name -> index
	var domains []domainEntry

	for i := range resolved {
		r := &resolved[i]
		if r.domainName == "" {
			continue
		}
		if _, exists := domainMap[r.domainName]; !exists {
			domainMap[r.domainName] = len(domains)
			domains = append(domains, domainEntry{name: r.domainName, sid: r.domainSID})
		}
	}

	// Build response stub data
	stubData := h.buildLookupSidsResponse(resolved, domains, domainMap, allMapped)

	resp := &Response{
		AllocHint:   uint32(len(stubData)),
		ContextID:   req.ContextID,
		CancelCount: 0,
		StubData:    stubData,
	}

	return resp.Encode(req.Header.CallID)
}

// parseSIDsFromRequest extracts SIDs from LookupSids2/3 request data.
// This is a simplified parser that handles the common case of 1-5 SIDs.
func (h *LSARPCHandler) parseSIDsFromRequest(data []byte, opnum uint16) []*sid.SID {
	var sids []*sid.SID

	// For opnum 57 (LookupSids2): skip policy handle (20 bytes) then SID array
	// For opnum 76 (LookupSids3): SID array directly
	offset := 0
	if opnum == OpLsarLookupSids2 {
		offset = 20 // Skip policy handle
	}

	// SID array structure:
	// DWORD Count (number of SIDs)
	// DWORD Pointer (to array)
	// DWORD MaxCount (conformant array max)
	// Then array of SID pointers
	// Then the actual SID data

	if offset+4 > len(data) {
		return nil
	}
	numSids := binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4

	if numSids == 0 || numSids > 100 {
		return nil
	}

	// Skip pointer to SID array
	if offset+4 > len(data) {
		return nil
	}
	offset += 4 // pointer

	// Conformant array max count
	if offset+4 > len(data) {
		return nil
	}
	offset += 4 // max count

	// Array of SID pointers (4 bytes each)
	ptrArrayEnd := offset + int(numSids)*4
	if ptrArrayEnd > len(data) {
		return nil
	}
	offset = ptrArrayEnd

	// Now parse the actual SID data. Each SID is preceded by:
	// SubAuthorityCount (4 bytes, conformant) then the SID binary data.
	for range numSids {
		if offset+4 > len(data) {
			break
		}
		// SubAuthorityCount as uint32 (NDR conformant)
		subAuthCount := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		sidSize := 8 + 4*int(subAuthCount)
		if offset+sidSize > len(data) {
			break
		}

		parsedSID, _, err := sid.DecodeSID(data[offset : offset+sidSize])
		if err == nil {
			sids = append(sids, parsedSID)
		}
		offset += sidSize
	}

	return sids
}

// domainEntry represents a referenced domain in LookupSids response.
type domainEntry struct {
	name string
	sid  *sid.SID
}

// buildLookupSidsResponse builds the NDR-encoded response for LookupSids2.
func (h *LSARPCHandler) buildLookupSidsResponse(
	resolved []resolvedSID,
	domains []domainEntry,
	domainMap map[string]int,
	allMapped bool,
) []byte {
	var buf bytes.Buffer

	// Referenced Domains
	// Pointer to LSAPR_REFERENCED_DOMAIN_LIST
	appendUint32Buf(&buf, 0x00020000) // non-null pointer

	// Entries count
	appendUint32Buf(&buf, uint32(len(domains)))

	// Pointer to array
	if len(domains) > 0 {
		appendUint32Buf(&buf, 0x00020004) // non-null pointer
	} else {
		appendUint32Buf(&buf, 0) // null pointer
	}

	// MaxEntries
	appendUint32Buf(&buf, uint32(len(domains)))

	if len(domains) > 0 {
		// Conformant array max count
		appendUint32Buf(&buf, uint32(len(domains)))

		// Fixed-size domain entries: name (NDR string header = 12 bytes) + SID pointer (4 bytes) = 16 bytes each
		for range domains {
			// Name: will be written as deferred data
			appendUint32Buf(&buf, 0)          // Length (placeholder, filled later or in deferred data)
			appendUint32Buf(&buf, 0)          // MaximumLength (placeholder)
			appendUint32Buf(&buf, 0x00020008) // pointer to string data
			// SID pointer
			appendUint32Buf(&buf, 0x0002000C) // pointer to SID data
		}

		// Deferred string data for each domain
		for _, d := range domains {
			writeNDRUnicodeString(&buf, d.name)
		}

		// Deferred SID data for each domain
		for _, d := range domains {
			if d.sid != nil {
				writeNDRSID(&buf, d.sid)
			} else {
				// Write a minimal SID (S-1-0-0) as placeholder
				writeNDRSID(&buf, sid.ParseSIDMust("S-1-0-0"))
			}
		}
	}

	// Translated Names
	// Count
	appendUint32Buf(&buf, uint32(len(resolved)))

	// Pointer to array
	if len(resolved) > 0 {
		appendUint32Buf(&buf, 0x00020010) // non-null pointer
	} else {
		appendUint32Buf(&buf, 0)
	}

	if len(resolved) > 0 {
		// Conformant array max count
		appendUint32Buf(&buf, uint32(len(resolved)))

		// Fixed-size name entries
		for _, r := range resolved {
			// Use type (uint16)
			_ = binary.Write(&buf, binary.LittleEndian, r.sidType)
			// Padding to 4-byte align
			_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
			// Name: NDR string header
			appendUint32Buf(&buf, 0)          // Length
			appendUint32Buf(&buf, 0)          // MaximumLength
			appendUint32Buf(&buf, 0x00020014) // pointer to string
			// Domain index (int32)
			domIdx := int32(-1) // -1 = no domain
			if r.domainName != "" {
				if idx, ok := domainMap[r.domainName]; ok {
					domIdx = int32(idx)
				}
			}
			_ = binary.Write(&buf, binary.LittleEndian, domIdx)
			// Flags (uint32)
			appendUint32Buf(&buf, 0)
		}

		// Deferred string data for each name
		for _, r := range resolved {
			writeNDRUnicodeString(&buf, r.name)
		}
	}

	// MappedCount
	mappedCount := uint32(0)
	for _, r := range resolved {
		if r.sidType != SidTypeUnknown {
			mappedCount++
		}
	}
	appendUint32Buf(&buf, mappedCount)

	// Return status
	if allMapped || len(resolved) == 0 {
		appendUint32Buf(&buf, statusSuccess)
	} else {
		appendUint32Buf(&buf, statusSomeNotMapped)
	}

	return buf.Bytes()
}

// writeNDRUnicodeString writes an NDR-conformant+varying unicode string.
func writeNDRUnicodeString(buf *bytes.Buffer, s string) {
	utf16 := encodeUTF16LE(s)
	charCount := uint32(len(s) + 1) // Include null terminator

	// Conformant: MaxCount
	appendUint32Buf(buf, charCount)
	// Varying: Offset
	appendUint32Buf(buf, 0)
	// Varying: ActualCount
	appendUint32Buf(buf, charCount)

	// UTF-16LE string data
	buf.Write(utf16)
	// Null terminator (2 bytes)
	buf.Write([]byte{0, 0})

	// Pad to 4-byte boundary
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// writeNDRSID writes an NDR-encoded SID.
func writeNDRSID(buf *bytes.Buffer, s *sid.SID) {
	// SubAuthorityCount as conformant max count
	appendUint32Buf(buf, uint32(s.SubAuthorityCount))
	// SID binary data
	sid.EncodeSID(buf, s)
	// Pad to 4-byte boundary
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// appendUint32Buf appends a little-endian uint32 to the buffer.
func appendUint32Buf(buf *bytes.Buffer, v uint32) {
	_ = binary.Write(buf, binary.LittleEndian, v)
}

// buildFault builds a DCE/RPC fault response.
func (h *LSARPCHandler) buildFault(callID uint32, status uint32) []byte {
	fragLen := HeaderSize + 16

	hdr := Header{
		VersionMajor: 5,
		VersionMinor: 0,
		PacketType:   PDUFault,
		Flags:        FlagFirstFrag | FlagLastFrag,
		DataRep:      [4]byte{0x10, 0x00, 0x00, 0x00},
		FragLength:   uint16(fragLen),
		AuthLength:   0,
		CallID:       callID,
	}

	buf := make([]byte, fragLen)
	copy(buf[0:16], hdr.Encode())
	binary.LittleEndian.PutUint32(buf[16:20], 0)      // alloc_hint
	binary.LittleEndian.PutUint16(buf[20:22], 0)      // context_id
	buf[22] = 0                                       // cancel_count
	buf[23] = 0                                       // reserved
	binary.LittleEndian.PutUint32(buf[24:28], status) // status
	binary.LittleEndian.PutUint32(buf[28:32], 0)      // reserved

	return buf
}
