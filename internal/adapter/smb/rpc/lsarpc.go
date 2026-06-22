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
	OpLsarOpenPolicy  uint16 = 6  // LsarOpenPolicy (legacy; smbcacls/older clients use this)
	OpLsarOpenPolicy2 uint16 = 44 // LsarOpenPolicy2
	OpLsarLookupSids  uint16 = 15 // LsarLookupSids  (legacy; smbcacls/rpcclient lookupsids)
	OpLsarLookupSids2 uint16 = 57 // LsarLookupSids2 (Windows Explorer)
	OpLsarLookupSids3 uint16 = 76 // LsarLookupSids3 (rpcclient lookupsids3)
)

// DCE/RPC reject status codes [C706 §14.6 / MS-RPCE]. Returned in a FAULT PDU.
const (
	// ncaSOpRngError is nca_s_op_rng_error — the requested operation number is
	// outside the range supported by the interface (unimplemented opnum).
	ncaSOpRngError uint32 = 0x1C010002
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
// Identity Resolution
// =============================================================================

// IdentityResolver resolves Unix UIDs/GIDs to human-readable names
// from the control plane store. When nil, the handler falls back to
// generic "unix_user:{uid}" / "unix_group:{gid}" names.
type IdentityResolver interface {
	LookupUsernameByUID(uid uint32) (string, bool)
	LookupGroupNameByGID(gid uint32) (string, bool)
}

// ForeignSIDResolver resolves a FOREIGN (AD/LDAP) domain SID — one that is
// neither well-known nor an algorithmic machine-domain SID — to its account
// name, NetBIOS domain, and SID type. It backs the directory side of the
// LSARPC SID-to-name path so Windows Explorer's Security tab shows real AD
// account names (e.g. CONTOSO\alice) instead of raw SIDs.
//
// Implementations consult the configured identity directory (the LDAP/AD
// provider via the centralized identity.Resolver) and SHOULD cache. A miss
// (ok=false) is expected and never an error: the SID stays unmapped and the
// batch returns STATUS_SOME_NOT_MAPPED. When nil, the handler skips the
// directory lookup entirely and foreign SIDs render as raw strings.
type ForeignSIDResolver interface {
	// LookupSID resolves a canonical SID string ("S-1-5-21-...") to its
	// account name and NetBIOS domain. sidType is one of the SidType*
	// constants (SidTypeUser / SidTypeGroup). ok=false means no directory
	// object matched the SID.
	LookupSID(sidString string) (name, domain string, sidType uint16, ok bool)

	// LookupUID resolves a POSIX UID to its directory account name and NetBIOS
	// domain. It backs the OWNER-SID display path for an AD-only user: the
	// machine-domain (algorithmic) SID on a file decodes to a UID that has no
	// local DittoFS account, so the name is recovered from the directory by its
	// uidNumber. ok=false means no directory object matched the UID.
	LookupUID(uid uint32) (name, domain string, ok bool)

	// LookupGID resolves a POSIX GID to its directory group name and NetBIOS
	// domain. Mirrors LookupUID for the GROUP SID on a file.
	LookupGID(gid uint32) (name, domain string, ok bool)
}

// =============================================================================
// LSA Handler
// =============================================================================

// LSARPCHandler handles LSA RPC calls for SID-to-name resolution.
type LSARPCHandler struct {
	sidMapper *sid.SIDMapper
	resolver  IdentityResolver

	// machineDomainName is the NetBIOS name advertised for algorithmic
	// machine-domain SIDs (the local user/group RIDs). Defaults to "DITTOFS"
	// when empty so existing standalone behavior is preserved.
	machineDomainName string

	// foreignResolver resolves AD/LDAP domain SIDs that are neither well-known
	// nor machine-domain. Optional; nil leaves foreign SIDs unmapped (raw).
	foreignResolver ForeignSIDResolver
}

// defaultMachineDomainName is the NetBIOS domain reported for machine-domain
// (algorithmic local UID/GID) SIDs when no explicit name is configured.
const defaultMachineDomainName = "DITTOFS"

// NewLSARPCHandler creates a new LSA RPC handler with the given SID mapper
// and optional identity resolver for real name resolution.
func NewLSARPCHandler(sidMapper *sid.SIDMapper, resolver IdentityResolver) *LSARPCHandler {
	return &LSARPCHandler{
		sidMapper:         sidMapper,
		resolver:          resolver,
		machineDomainName: defaultMachineDomainName,
	}
}

// SetMachineDomainName overrides the NetBIOS domain name reported for
// machine-domain SIDs. An empty value resets it to the default ("DITTOFS").
func (h *LSARPCHandler) SetMachineDomainName(name string) {
	if name == "" {
		h.machineDomainName = defaultMachineDomainName
		return
	}
	h.machineDomainName = name
}

// SetForeignSIDResolver wires the directory-backed resolver used for AD/LDAP
// domain SIDs. Safe to leave unset; foreign SIDs then render as raw strings.
func (h *LSARPCHandler) SetForeignSIDResolver(r ForeignSIDResolver) {
	h.foreignResolver = r
}

// machineDomainNameOrDefault returns the configured machine NetBIOS name,
// falling back to the default for a zero-valued handler (e.g. struct literals
// in tests that bypass NewLSARPCHandler).
func (h *LSARPCHandler) machineDomainNameOrDefault() string {
	if h.machineDomainName == "" {
		return defaultMachineDomainName
	}
	return h.machineDomainName
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
	case OpLsarOpenPolicy, OpLsarOpenPolicy2:
		return h.handleOpenPolicy(req)
	case OpLsarLookupSids, OpLsarLookupSids2, OpLsarLookupSids3:
		return h.handleLookupSids(req)
	default:
		return h.buildFault(req.Header.CallID, ncaSOpRngError)
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

// handleOpenPolicy handles both LsarOpenPolicy (opnum 6) and LsarOpenPolicy2
// (opnum 44). Both return a policy handle required before LookupSids; the
// request stubs differ only in the system-name argument, which we ignore.
// smbcacls and older clients call the legacy opnum 6 first — without this,
// they receive a fault and fall back to displaying raw SIDs.
func (h *LSARPCHandler) handleOpenPolicy(req *Request) []byte {
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
		sidType := SidTypeWellKnownGroup

		if localName, ok := strings.CutPrefix(name, "NT AUTHORITY\\"); ok {
			return resolvedSID{
				name:       localName,
				sidType:    sidType,
				domainName: "NT AUTHORITY",
				domainSID:  sid.ParseSIDMust("S-1-5"),
			}
		}
		if localName, ok := strings.CutPrefix(name, "BUILTIN\\"); ok {
			return resolvedSID{
				name:       localName,
				sidType:    SidTypeAlias,
				domainName: "BUILTIN",
				domainSID:  sid.ParseSIDMust("S-1-5-32"),
			}
		}
		// No domain prefix (e.g., "Everyone")
		return resolvedSID{name: name, sidType: sidType}
	}

	// Check machine domain SIDs (user). The local control-plane store is tried
	// first (LookupUsernameByUID). When it misses — the common AD case where the
	// file owner is an AD-only user (e.g. alice, uid 10001) with no local DittoFS
	// account — fall back to the directory by uidNumber so Explorer still shows
	// "DOMAIN\alice" instead of a synthetic "unix_user:10001". The owner SID on
	// the wire is the machine-domain algorithmic SID, NOT alice's real AD SID, so
	// this reverse-UID path (not the foreign-SID path) is what names it.
	if uid, ok := h.sidMapper.UIDFromSID(s); ok {
		if h.resolver != nil {
			if name, found := h.resolver.LookupUsernameByUID(uid); found {
				return resolvedSID{
					name:       name,
					sidType:    SidTypeUser,
					domainName: h.machineDomainNameOrDefault(),
					domainSID:  h.machineDomainSID(),
				}
			}
		}
		if h.foreignResolver != nil {
			if name, domain, found := h.foreignResolver.LookupUID(uid); found {
				return resolvedSID{
					name:       name,
					sidType:    SidTypeUser,
					domainName: domain,
					domainSID:  h.machineDomainSID(),
				}
			}
		}
		return resolvedSID{
			name:       fmt.Sprintf("unix_user:%d", uid),
			sidType:    SidTypeUser,
			domainName: h.machineDomainNameOrDefault(),
			domainSID:  h.machineDomainSID(),
		}
	}

	// Check machine domain SIDs (group). Same local-then-directory fallback as
	// the user branch above, by gidNumber.
	if gid, ok := h.sidMapper.GIDFromSID(s); ok {
		if h.resolver != nil {
			if name, found := h.resolver.LookupGroupNameByGID(gid); found {
				return resolvedSID{
					name:       name,
					sidType:    SidTypeGroup,
					domainName: h.machineDomainNameOrDefault(),
					domainSID:  h.machineDomainSID(),
				}
			}
		}
		if h.foreignResolver != nil {
			if name, domain, found := h.foreignResolver.LookupGID(gid); found {
				return resolvedSID{
					name:       name,
					sidType:    SidTypeGroup,
					domainName: domain,
					domainSID:  h.machineDomainSID(),
				}
			}
		}
		return resolvedSID{
			name:       fmt.Sprintf("unix_group:%d", gid),
			sidType:    SidTypeGroup,
			domainName: h.machineDomainNameOrDefault(),
			domainSID:  h.machineDomainSID(),
		}
	}

	// Foreign (AD/LDAP) domain SID: not well-known, not machine-domain. Consult
	// the directory-backed resolver. A hit yields the real AD account name + its
	// NetBIOS domain; a miss falls through to the unknown branch below so the SID
	// stays unmapped (STATUS_SOME_NOT_MAPPED) — never a fault.
	if h.foreignResolver != nil {
		sidStr := sid.FormatSID(s)
		if name, domain, sidType, ok := h.foreignResolver.LookupSID(sidStr); ok {
			if sidType != SidTypeUser && sidType != SidTypeGroup && sidType != SidTypeDomain &&
				sidType != SidTypeAlias && sidType != SidTypeWellKnownGroup {
				sidType = SidTypeUser
			}
			return resolvedSID{
				name:       name,
				sidType:    sidType,
				domainName: domain,
				domainSID:  foreignDomainSID(s),
			}
		}
	}

	// Unknown SID
	return resolvedSID{
		name:    sid.FormatSID(s),
		sidType: SidTypeUnknown,
	}
}

// foreignDomainSID derives the domain SID (the account SID with its trailing
// RID stripped) for a foreign account SID. For a standard 5-sub-authority
// S-1-5-21-{a}-{b}-{c}-{RID} SID this yields S-1-5-21-{a}-{b}-{c}. SIDs without
// a strippable RID are returned unchanged so the referenced-domain entry stays
// well-formed.
func foreignDomainSID(s *sid.SID) *sid.SID {
	if s == nil || s.SubAuthorityCount < 1 {
		return s
	}
	n := int(s.SubAuthorityCount) - 1
	return &sid.SID{
		Revision:            s.Revision,
		SubAuthorityCount:   byte(n),
		IdentifierAuthority: s.IdentifierAuthority,
		SubAuthorities:      append([]uint32(nil), s.SubAuthorities[:n]...),
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

// handleLookupSids handles LsarLookupSids (opnum 15), LsarLookupSids2 (opnum
// 57), and LsarLookupSids3 (opnum 76). It parses the SID array, resolves each
// SID, and returns a response with referenced domains and translated names.
//
// Per MS-LSAT the response translated-names structure is per-opnum:
//   - 15 (LsarLookupSids): LSAPR_TRANSLATED_NAMES — entries are
//     LSAPR_TRANSLATED_NAME with NO Flags field.
//   - 57/76 (LookupSids2/3): LSAPR_TRANSLATED_NAMES_EX — entries are
//     LSAPR_TRANSLATED_NAME_EX with a trailing Flags (ULONG). The 57 and 76
//     responses are byte-identical (they differ only in request-side params),
//     so both share the EX builder.
//
// Emitting the EX layout for opnum 15 (or vice-versa) shifts every field after
// the first entry by 4 bytes, which a real client parses as an out-of-bounds
// array — the NT_STATUS_ARRAY_BOUNDS_EXCEEDED seen from rpcclient.
func (h *LSARPCHandler) handleLookupSids(req *Request) []byte {
	// Parse SIDs from the request stub data. The leading bytes differ per opnum
	// (policy handle for 15/57, none for 76) — parseSIDsFromRequest accounts for it.
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

	// Build response stub data. Opnum 15 uses the non-EX translated-name layout
	// (no Flags); 57/76 use the EX layout.
	withFlags := req.OpNum != OpLsarLookupSids
	stubData := h.buildLookupSidsResponse(resolved, domains, domainMap, allMapped, withFlags)

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

	// LsarLookupSids (15) and LsarLookupSids2 (57) both lead with a 20-byte
	// LSAPR_HANDLE PolicyHandle before the SID_ENUM_BUFFER. LsarLookupSids3 (76)
	// takes an RPC binding handle (handle_t) instead, which is never marshaled,
	// so its SID_ENUM_BUFFER starts at offset 0.
	offset := 0
	if opnum == OpLsarLookupSids || opnum == OpLsarLookupSids2 {
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

// buildLookupSidsResponse builds the NDR-encoded LookupSids response.
//
// The referenced-domain list, mapped count, and status are identical across
// all three opnums. withFlags selects the translated-name entry layout: true
// emits LSAPR_TRANSLATED_NAME_EX (Use + Name + DomainIndex + Flags) for
// LookupSids2/3 (opnum 57/76); false emits LSAPR_TRANSLATED_NAME (no Flags)
// for the legacy LookupSids (opnum 15).
func (h *LSARPCHandler) buildLookupSidsResponse(
	resolved []resolvedSID,
	domains []domainEntry,
	domainMap map[string]int,
	allMapped bool,
	withFlags bool,
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
		refID := uint32(0x00020008)
		for _, d := range domains {
			nameUTF16Len := uint16(len(encodeUTF16LE(d.name)))
			byteLen := nameUTF16Len + 2                               // include null terminator
			_ = binary.Write(&buf, binary.LittleEndian, nameUTF16Len) // Length (excluding null terminator)
			_ = binary.Write(&buf, binary.LittleEndian, byteLen)      // MaximumLength (including null terminator)
			appendUint32Buf(&buf, refID)                              // pointer to string data
			refID += 4
			appendUint32Buf(&buf, refID) // pointer to SID data
			refID += 4
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
		nameRefID := uint32(0x00030000)
		for _, r := range resolved {
			_ = binary.Write(&buf, binary.LittleEndian, r.sidType)
			_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
			nameUTF16Len := uint16(len(encodeUTF16LE(r.name)))
			byteLen := nameUTF16Len + 2
			_ = binary.Write(&buf, binary.LittleEndian, nameUTF16Len) // Length (excluding null terminator)
			_ = binary.Write(&buf, binary.LittleEndian, byteLen)      // MaximumLength (including null terminator)
			appendUint32Buf(&buf, nameRefID)                          // unique pointer
			nameRefID += 4
			// Domain index (int32)
			domIdx := int32(-1) // -1 = no domain
			if r.domainName != "" {
				if idx, ok := domainMap[r.domainName]; ok {
					domIdx = int32(idx)
				}
			}
			_ = binary.Write(&buf, binary.LittleEndian, domIdx)
			// Flags (uint32) — present only in the _EX layout (opnum 57/76).
			if withFlags {
				appendUint32Buf(&buf, 0)
			}
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
	charCount := uint32(len(utf16)/2 + 1) // UTF-16 code units + null terminator

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
