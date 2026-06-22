package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// mockResolver is a test IdentityResolver that returns preset names.
type mockResolver struct {
	users  map[uint32]string
	groups map[uint32]string
}

func (m *mockResolver) LookupUsernameByUID(uid uint32) (string, bool) {
	name, ok := m.users[uid]
	return name, ok
}

func (m *mockResolver) LookupGroupNameByGID(gid uint32) (string, bool) {
	name, ok := m.groups[gid]
	return name, ok
}

// newTestLSAHandler creates an LSARPCHandler with a deterministic mapper and no resolver.
func newTestLSAHandler() *LSARPCHandler {
	return NewLSARPCHandler(sid.NewSIDMapper(0, 0, 0), nil)
}

// buildTestBindRequest creates a bind request for the LSA interface.
func buildTestBindRequest(callID uint32) []byte {
	// Build a minimal bind PDU for the LSA interface
	buf := make([]byte, 72)

	// Header
	buf[0] = 5 // version major
	buf[1] = 0 // version minor
	buf[2] = PDUBind
	buf[3] = FlagFirstFrag | FlagLastFrag
	buf[4] = 0x10                                     // data rep: little endian
	binary.LittleEndian.PutUint16(buf[8:10], 72)      // frag length
	binary.LittleEndian.PutUint32(buf[12:16], callID) // call ID

	// Bind body
	binary.LittleEndian.PutUint16(buf[16:18], 4280) // max xmit frag
	binary.LittleEndian.PutUint16(buf[18:20], 4280) // max recv frag
	binary.LittleEndian.PutUint32(buf[20:24], 0)    // assoc group
	buf[24] = 1                                     // num contexts
	// padding (3 bytes at 25-27)

	// Context entry
	binary.LittleEndian.PutUint16(buf[28:30], 0) // context ID
	buf[30] = 1                                  // num transfer syntaxes
	// padding (1 byte at 31)

	// Abstract syntax: LSA UUID
	copy(buf[32:48], LSAInterfaceUUID[:])
	binary.LittleEndian.PutUint32(buf[48:52], 0) // version

	// Transfer syntax: NDR
	copy(buf[52:68], NDRTransferSyntaxUUID[:])
	binary.LittleEndian.PutUint32(buf[68:72], 2) // version

	return buf
}

// buildTestRequest creates an RPC request PDU.
func buildTestRequest(callID uint32, opnum uint16, stubData []byte) []byte {
	fragLen := HeaderSize + 8 + len(stubData)
	buf := make([]byte, fragLen)

	// Header
	buf[0] = 5
	buf[1] = 0
	buf[2] = PDURequest
	buf[3] = FlagFirstFrag | FlagLastFrag
	buf[4] = 0x10 // data rep
	binary.LittleEndian.PutUint16(buf[8:10], uint16(fragLen))
	binary.LittleEndian.PutUint32(buf[12:16], callID)

	// Request body
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(stubData))) // alloc hint
	binary.LittleEndian.PutUint16(buf[20:22], 0)                     // context ID
	binary.LittleEndian.PutUint16(buf[22:24], opnum)

	copy(buf[24:], stubData)

	return buf
}

func TestLSARPC_Bind(t *testing.T) {
	h := newTestLSAHandler()

	bindData := buildTestBindRequest(1)
	bindReq, err := ParseBindRequest(bindData)
	if err != nil {
		t.Fatalf("ParseBindRequest: %v", err)
	}

	response := h.HandleBind(bindReq)
	if len(response) < HeaderSize {
		t.Fatalf("Bind response too short: %d bytes", len(response))
	}

	// Verify it's a bind ack
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUBindAck {
		t.Errorf("Response type = %d, want %d (BindAck)", hdr.PacketType, PDUBindAck)
	}
	if hdr.CallID != 1 {
		t.Errorf("CallID = %d, want 1", hdr.CallID)
	}
}

func TestLSARPC_OpenPolicy2(t *testing.T) {
	h := newTestLSAHandler()

	// OpenPolicy2 stub data (minimal - just needs to be parseable)
	stubData := make([]byte, 48)
	reqData := buildTestRequest(2, OpLsarOpenPolicy2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	// Verify it's a response PDU
	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// Extract stub data: starts at offset 24
	if len(response) < 24+24 {
		t.Fatalf("Response stub data too short for OpenPolicy2 response")
	}

	// Status is at offset 20 within stub data (after 20-byte policy handle)
	stubStart := 24
	status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
	if status != statusSuccess {
		t.Errorf("OpenPolicy2 status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
	}
}

// TestLSARPC_OpenPolicy verifies the legacy LsarOpenPolicy (opnum 6) returns a
// success policy handle, not a fault. smbcacls and older clients call opnum 6
// before LookupSids; faulting it makes them fall back to raw SIDs (#1291).
func TestLSARPC_OpenPolicy(t *testing.T) {
	h := newTestLSAHandler()

	// LsarOpenPolicy stub data (minimal - handler ignores it).
	stubData := make([]byte, 48)
	reqData := buildTestRequest(2, OpLsarOpenPolicy, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Fatalf("LsarOpenPolicy (opnum 6) returned PDU type %d, want %d (Response) — clients fall back to raw SIDs on a fault", hdr.PacketType, PDUResponse)
	}

	// Status is at offset 20 within stub data (after 20-byte policy handle).
	if len(response) < 24+24 {
		t.Fatalf("Response stub data too short for OpenPolicy response")
	}
	stubStart := 24
	status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
	if status != statusSuccess {
		t.Errorf("LsarOpenPolicy status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
	}
}

func TestLSARPC_Close(t *testing.T) {
	h := newTestLSAHandler()

	// Close stub data: 20-byte policy handle
	stubData := make([]byte, 20)
	stubData[0] = 0x01 // non-zero handle
	reqData := buildTestRequest(3, OpLsarClose, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// Verify success status
	stubStart := 24
	if len(response) >= stubStart+24 {
		status := binary.LittleEndian.Uint32(response[stubStart+20 : stubStart+24])
		if status != statusSuccess {
			t.Errorf("Close status = 0x%08x, want 0x%08x (success)", status, statusSuccess)
		}
	}
}

// buildLookupSids2StubData builds a minimal LookupSids2 request with the given SIDs.
func buildLookupSids2StubData(sids []*sid.SID) []byte {
	var buf bytes.Buffer

	// Policy handle (20 bytes)
	policyHandle := make([]byte, 20)
	policyHandle[0] = 0x01
	buf.Write(policyHandle)

	// SID array: Count
	appendUint32Buf(&buf, uint32(len(sids)))
	// Pointer to array
	appendUint32Buf(&buf, 0x00020000)
	// Conformant array max count
	appendUint32Buf(&buf, uint32(len(sids)))

	// SID pointers
	for range sids {
		appendUint32Buf(&buf, 0x00020004)
	}

	// SID data
	for _, s := range sids {
		// SubAuthorityCount as conformant max
		appendUint32Buf(&buf, uint32(s.SubAuthorityCount))
		// SID binary
		sid.EncodeSID(&buf, s)
	}

	return buf.Bytes()
}

func TestLSARPC_LookupSids2_WellKnown(t *testing.T) {
	h := newTestLSAHandler()

	// Look up "Everyone" SID (S-1-1-0)
	sids := []*sid.SID{sid.WellKnownEveryone}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(4, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Errorf("Response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}

	// The response should contain a valid stub with the resolved name.
	// We verify the response is long enough and doesn't have a fault status.
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}

	// Verify the response stub data is non-empty
	if len(response) <= 24 {
		t.Fatal("Response stub data is empty")
	}
}

func TestLSARPC_LookupSids2_DomainUser(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, nil)

	// Look up domain user SID for UID 1000
	userSID := mapper.UserSID(1000)
	sids := []*sid.SID{userSID}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(5, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}
}

func TestLSARPC_LookupSids2_Unknown(t *testing.T) {
	h := newTestLSAHandler()

	// Look up an unknown SID
	unknownSID := sid.ParseSIDMust("S-1-99-42")
	sids := []*sid.SID{unknownSID}
	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(6, OpLsarLookupSids2, stubData)

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize+8 {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("Got fault response instead of success")
	}

	// Verify status indicates some not mapped (since the SID is unknown)
	// Status is the last 4 bytes of stub data
	stubData2 := response[24:]
	if len(stubData2) >= 4 {
		status := binary.LittleEndian.Uint32(stubData2[len(stubData2)-4:])
		if status != statusSomeNotMapped && status != statusSuccess {
			t.Errorf("Status = 0x%08x, want STATUS_SOME_NOT_MAPPED (0x%08x) or success",
				status, statusSomeNotMapped)
		}
	}
}

func TestLSARPC_UnsupportedOpnum(t *testing.T) {
	h := newTestLSAHandler()

	stubData := make([]byte, 16)
	reqData := buildTestRequest(7, 99, stubData) // opnum 99 is unsupported

	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}

	response := h.HandleRequest(req)
	if len(response) < HeaderSize {
		t.Fatalf("Response too short: %d bytes", len(response))
	}

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUFault {
		t.Errorf("Expected fault for unsupported opnum, got type %d", hdr.PacketType)
	}
}

func TestIsSupportedPipe_LSARPC(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"lsarpc", true},
		{"\\lsarpc", true},
		{"\\pipe\\lsarpc", true},
		{"srvsvc", true},
		{"\\srvsvc", true},
		{"\\pipe\\srvsvc", true},
		{"unknown", false},
		{"samr", false},
	}

	for _, tt := range tests {
		result := IsSupportedPipe(tt.name)
		if result != tt.expected {
			t.Errorf("IsSupportedPipe(%q) = %v, want %v", tt.name, result, tt.expected)
		}
	}
}

func TestPipeManager_CreateLSARPCPipe(t *testing.T) {
	pm := NewPipeManager()
	mapper := sid.NewSIDMapper(111, 222, 333)
	pm.SetSIDMapper(mapper)

	fileID := [16]byte{1, 2, 3, 4}
	pipe := pm.CreatePipe(fileID, "lsarpc")

	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}
	if pipe.Name != "lsarpc" {
		t.Errorf("pipe.Name = %q, want lsarpc", pipe.Name)
	}

	// Verify the pipe has an LSARPCHandler
	_, ok := pipe.Handler.(*LSARPCHandler)
	if !ok {
		t.Errorf("pipe.Handler type = %T, want *LSARPCHandler", pipe.Handler)
	}

	// Verify the pipe can be retrieved
	retrieved := pm.GetPipe(fileID)
	if retrieved != pipe {
		t.Error("GetPipe returned different pipe")
	}
}

func TestPipeManager_CreateSRVSVCPipe(t *testing.T) {
	pm := NewPipeManager()
	pm.SetShares([]ShareInfo1{{Name: "test", Type: STYPE_DISKTREE}})

	fileID := [16]byte{5, 6, 7, 8}
	pipe := pm.CreatePipe(fileID, "srvsvc")

	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}

	// Verify the pipe has an SRVSVCHandler
	_, ok := pipe.Handler.(*SRVSVCHandler)
	if !ok {
		t.Errorf("pipe.Handler type = %T, want *SRVSVCHandler", pipe.Handler)
	}
}

func TestResolveSID_WellKnown(t *testing.T) {
	h := newTestLSAHandler()

	tests := []struct {
		sid      *sid.SID
		wantName string
		wantType uint16
	}{
		{sid.WellKnownEveryone, "Everyone", SidTypeWellKnownGroup},
		{sid.WellKnownSystem, "SYSTEM", SidTypeWellKnownGroup},
		{sid.WellKnownAdministrators, "Administrators", SidTypeAlias},
		{sid.WellKnownAnonymous, "ANONYMOUS LOGON", SidTypeWellKnownGroup},
	}

	for _, tt := range tests {
		r := h.resolveSID(tt.sid)
		if r.name != tt.wantName {
			t.Errorf("resolveSID(%s).name = %q, want %q", sid.FormatSID(tt.sid), r.name, tt.wantName)
		}
		if r.sidType != tt.wantType {
			t.Errorf("resolveSID(%s).sidType = %d, want %d", sid.FormatSID(tt.sid), r.sidType, tt.wantType)
		}
	}
}

func TestResolveSID_DomainUser_NoResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, nil)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "unix_user:42" {
		t.Errorf("resolveSID(user 42).name = %q, want unix_user:42", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("resolveSID(user 42).sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(user 42).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainUser_WithResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	resolver := &mockResolver{
		users: map[uint32]string{42: "alice"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "alice" {
		t.Errorf("resolveSID(user 42).name = %q, want alice", r.name)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("resolveSID(user 42).sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(user 42).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainGroup_NoResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, nil)

	groupSID := mapper.GroupSID(100)
	r := h.resolveSID(groupSID)

	if r.name != "unix_group:100" {
		t.Errorf("resolveSID(group 100).name = %q, want unix_group:100", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("resolveSID(group 100).sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(group 100).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainGroup_WithResolver(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	resolver := &mockResolver{
		groups: map[uint32]string{100: "editors"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	groupSID := mapper.GroupSID(100)
	r := h.resolveSID(groupSID)

	if r.name != "editors" {
		t.Errorf("resolveSID(group 100).name = %q, want editors", r.name)
	}
	if r.sidType != SidTypeGroup {
		t.Errorf("resolveSID(group 100).sidType = %d, want %d (Group)", r.sidType, SidTypeGroup)
	}
	if r.domainName != "DITTOFS" {
		t.Errorf("resolveSID(group 100).domainName = %q, want DITTOFS", r.domainName)
	}
}

func TestResolveSID_DomainUser_ResolverMiss(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	// Resolver has no entry for UID 42 — should fallback to generic name
	resolver := &mockResolver{
		users: map[uint32]string{999: "bob"},
	}
	h := NewLSARPCHandler(mapper, resolver)

	userSID := mapper.UserSID(42)
	r := h.resolveSID(userSID)

	if r.name != "unix_user:42" {
		t.Errorf("resolveSID(user 42).name = %q, want unix_user:42", r.name)
	}
}

func TestResolveSID_Unknown(t *testing.T) {
	h := newTestLSAHandler()

	unknownSID := sid.ParseSIDMust("S-1-99-42")
	r := h.resolveSID(unknownSID)

	if r.sidType != SidTypeUnknown {
		t.Errorf("resolveSID(unknown).sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
	// Name should be the SID string for unknown SIDs
	if r.name != "S-1-99-42" {
		t.Errorf("resolveSID(unknown).name = %q, want S-1-99-42", r.name)
	}
}

func TestPipeManager_IdentityResolver(t *testing.T) {
	pm := NewPipeManager()
	mapper := sid.NewSIDMapper(0, 0, 0)
	pm.SetSIDMapper(mapper)

	resolver := &mockResolver{
		users:  map[uint32]string{1000: "alice"},
		groups: map[uint32]string{1000: "users"},
	}
	pm.SetIdentityResolver(resolver)

	fileID := [16]byte{10, 11, 12}
	pipe := pm.CreatePipe(fileID, "lsarpc")
	if pipe == nil {
		t.Fatal("CreatePipe returned nil")
	}

	lsaHandler, ok := pipe.Handler.(*LSARPCHandler)
	if !ok {
		t.Fatalf("pipe.Handler type = %T, want *LSARPCHandler", pipe.Handler)
	}

	// Verify resolver is wired through
	if lsaHandler.resolver == nil {
		t.Fatal("LSARPCHandler.resolver is nil, expected resolver to be set")
	}

	// Verify resolver works end-to-end
	userSID := mapper.UserSID(1000)
	r := lsaHandler.resolveSID(userSID)
	if r.name != "alice" {
		t.Errorf("resolveSID(user 1000).name = %q, want alice", r.name)
	}
}

func TestStoreIdentityResolver(t *testing.T) {
	r := &StoreIdentityResolver{
		LookupUser: func(uid uint32) (string, bool) {
			if uid == 1000 {
				return "alice", true
			}
			return "", false
		},
		LookupGroup: func(gid uint32) (string, bool) {
			if gid == 1000 {
				return "users", true
			}
			return "", false
		},
	}

	if name, ok := r.LookupUsernameByUID(1000); !ok || name != "alice" {
		t.Errorf("LookupUsernameByUID(1000) = (%q, %v), want (alice, true)", name, ok)
	}
	if _, ok := r.LookupUsernameByUID(9999); ok {
		t.Error("LookupUsernameByUID(9999) should return false")
	}
	if name, ok := r.LookupGroupNameByGID(1000); !ok || name != "users" {
		t.Errorf("LookupGroupNameByGID(1000) = (%q, %v), want (users, true)", name, ok)
	}
	if _, ok := r.LookupGroupNameByGID(9999); ok {
		t.Error("LookupGroupNameByGID(9999) should return false")
	}
}

func TestStoreIdentityResolver_NilFuncs(t *testing.T) {
	r := &StoreIdentityResolver{}

	if _, ok := r.LookupUsernameByUID(1000); ok {
		t.Error("LookupUsernameByUID should return false when LookupUser is nil")
	}
	if _, ok := r.LookupGroupNameByGID(1000); ok {
		t.Error("LookupGroupNameByGID should return false when LookupGroup is nil")
	}
}

// =============================================================================
// Foreign (AD/LDAP) SID resolution
// =============================================================================

// mockForeignResolver is a test ForeignSIDResolver keyed by canonical SID string.
type mockForeignResolver struct {
	hits map[string]foreignHit
}

type foreignHit struct {
	name    string
	domain  string
	sidType uint16
}

func (m *mockForeignResolver) LookupSID(sidString string) (string, string, uint16, bool) {
	h, ok := m.hits[sidString]
	if !ok {
		return "", "", 0, false
	}
	return h.name, h.domain, h.sidType, true
}

func TestResolveSID_ForeignAD_Hit(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	resolver := &mockForeignResolver{
		hits: map[string]foreignHit{
			sid.FormatSID(adSID): {name: "alice", domain: "CONTOSO", sidType: SidTypeUser},
		},
	}
	h := NewLSARPCHandler(mapper, nil)
	h.SetForeignSIDResolver(resolver)

	r := h.resolveSID(adSID)
	if r.name != "alice" {
		t.Errorf("name = %q, want alice", r.name)
	}
	if r.domainName != "CONTOSO" {
		t.Errorf("domainName = %q, want CONTOSO", r.domainName)
	}
	if r.sidType != SidTypeUser {
		t.Errorf("sidType = %d, want %d (User)", r.sidType, SidTypeUser)
	}
	// The referenced domain SID must be the account SID with its RID stripped.
	wantDomSID := sid.FormatSID(sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333"))
	if r.domainSID == nil || sid.FormatSID(r.domainSID) != wantDomSID {
		t.Errorf("domainSID = %v, want %s", r.domainSID, wantDomSID)
	}
}

func TestResolveSID_ForeignAD_Miss(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	resolver := &mockForeignResolver{hits: map[string]foreignHit{}} // empty: always misses
	h := NewLSARPCHandler(mapper, nil)
	h.SetForeignSIDResolver(resolver)

	r := h.resolveSID(adSID)
	if r.sidType != SidTypeUnknown {
		t.Errorf("miss sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
	if r.name != sid.FormatSID(adSID) {
		t.Errorf("miss name = %q, want raw SID %q", r.name, sid.FormatSID(adSID))
	}
}

func TestResolveSID_ForeignAD_NilResolver(t *testing.T) {
	// With no foreign resolver wired, a foreign SID is unknown (raw), never a panic.
	h := newTestLSAHandler()
	adSID := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	r := h.resolveSID(adSID)
	if r.sidType != SidTypeUnknown {
		t.Errorf("sidType = %d, want %d (Unknown)", r.sidType, SidTypeUnknown)
	}
}

func TestResolveSID_MachineDomainName_Configurable(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	h := NewLSARPCHandler(mapper, &mockResolver{users: map[uint32]string{7: "bob"}})

	// Default machine domain name.
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "DITTOFS" {
		t.Errorf("default domainName = %q, want DITTOFS", got)
	}

	// Overridden machine domain name applies to machine-domain SIDs only.
	h.SetMachineDomainName("FILER01")
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "FILER01" {
		t.Errorf("overridden domainName = %q, want FILER01", got)
	}

	// Resetting with empty restores the default.
	h.SetMachineDomainName("")
	if got := h.resolveSID(mapper.UserSID(7)).domainName; got != "DITTOFS" {
		t.Errorf("reset domainName = %q, want DITTOFS", got)
	}
}

// lookupSidsResult is a decoded LsarLookupSids response (the parts under test).
type lookupSidsResult struct {
	domains     []string // referenced-domain names, in list order
	names       []translatedName
	mappedCount uint32
	status      uint32
}

type translatedName struct {
	sidType   uint16
	domainIdx int32
}

// parseLookupSidsResponse decodes the stub data produced by
// buildLookupSidsResponse. It mirrors that exact layout — it is a test-only
// reader, not a general NDR parser.
func parseLookupSidsResponse(t *testing.T, response []byte) lookupSidsResult {
	t.Helper()
	if len(response) <= 24 {
		t.Fatalf("response too short: %d bytes", len(response))
	}
	stub := response[24:]
	off := 0
	u32 := func() uint32 {
		v := binary.LittleEndian.Uint32(stub[off : off+4])
		off += 4
		return v
	}
	u16 := func() uint16 {
		v := binary.LittleEndian.Uint16(stub[off : off+2])
		off += 2
		return v
	}

	var res lookupSidsResult

	// --- Referenced domain list ---
	_ = u32() // list pointer
	domCount := u32()
	_ = u32() // array pointer
	_ = u32() // max entries

	type domFixed struct{ nameUTF16Bytes uint16 }
	var domFixeds []domFixed
	if domCount > 0 {
		_ = u32() // conformant max count
		for range domCount {
			length := u16()      // Length (excl null), in bytes
			_ = u16()            // MaximumLength
			_ = u32()            // string pointer
			_ = u32()            // SID pointer
			domFixeds = append(domFixeds, domFixed{nameUTF16Bytes: length})
		}
		// Deferred domain name strings.
		for _, d := range domFixeds {
			name := readNDRUnicodeString(t, stub, &off)
			_ = d
			res.domains = append(res.domains, name)
		}
		// Deferred domain SIDs.
		for range domCount {
			skipNDRSID(t, stub, &off)
		}
	}

	// --- Translated names ---
	nameCount := u32()
	_ = u32() // array pointer
	if nameCount > 0 {
		_ = u32() // conformant max count
		for range nameCount {
			st := u16()
			_ = u16() // pad/reserved
			_ = u16() // Length
			_ = u16() // MaximumLength
			_ = u32() // name pointer
			domIdx := int32(u32())
			_ = u32() // flags
			res.names = append(res.names, translatedName{sidType: st, domainIdx: domIdx})
		}
		// Deferred name strings.
		for range nameCount {
			readNDRUnicodeString(t, stub, &off)
		}
	}

	res.mappedCount = u32()
	res.status = u32()
	return res
}

// readNDRUnicodeString decodes a writeNDRUnicodeString-encoded string and
// advances off past the 4-byte padding.
func readNDRUnicodeString(t *testing.T, b []byte, off *int) string {
	t.Helper()
	maxCount := binary.LittleEndian.Uint32(b[*off : *off+4])
	*off += 4
	*off += 4 // offset
	actual := binary.LittleEndian.Uint32(b[*off : *off+4])
	*off += 4
	_ = maxCount
	// actual includes the null terminator (charCount).
	chars := int(actual)
	byteLen := chars * 2
	data := b[*off : *off+byteLen]
	*off += byteLen
	// 4-byte align.
	for *off%4 != 0 {
		*off++
	}
	// Drop the trailing null code unit and decode UTF-16LE.
	var sb []rune
	for i := 0; i+1 < len(data); i += 2 {
		c := binary.LittleEndian.Uint16(data[i : i+2])
		if c == 0 {
			break
		}
		sb = append(sb, rune(c))
	}
	return string(sb)
}

// skipNDRSID advances off past a writeNDRSID-encoded SID.
func skipNDRSID(t *testing.T, b []byte, off *int) {
	t.Helper()
	_ = binary.LittleEndian.Uint32(b[*off : *off+4]) // conformant max count
	*off += 4
	// SID: revision(1) + subauthcount(1) + authority(6) + subauths(4*n)
	subCount := int(b[*off+1])
	*off += 8 + 4*subCount
	for *off%4 != 0 {
		*off++
	}
}

// TestLookupSids_MixedBatch_MultiDomain exercises a batch containing a
// well-known SID, a machine-domain user, a foreign AD hit, and a foreign AD
// miss. It asserts STATUS_SOME_NOT_MAPPED, the correct MappedCount, per-SID
// types, and that the referenced-domain list correctly carries BUILTIN/the
// machine domain/the AD domain with the per-name domain index pointing at the
// right entry.
func TestLookupSids_MixedBatch_MultiDomain(t *testing.T) {
	mapper := sid.NewSIDMapper(0, 0, 0)
	localResolver := &mockResolver{users: map[uint32]string{5: "localuser"}}

	adHit := sid.ParseSIDMust("S-1-5-21-1111111111-2222222222-3333333333-1107")
	adMiss := sid.ParseSIDMust("S-1-5-21-999999999-888888888-777777777-1500")
	foreign := &mockForeignResolver{
		hits: map[string]foreignHit{
			sid.FormatSID(adHit): {name: "alice", domain: "CONTOSO", sidType: SidTypeUser},
		},
	}

	h := NewLSARPCHandler(mapper, localResolver)
	h.SetForeignSIDResolver(foreign)
	h.SetMachineDomainName("FILER01")

	sids := []*sid.SID{
		sid.WellKnownAdministrators, // BUILTIN\Administrators (alias)
		mapper.UserSID(5),           // FILER01\localuser
		adHit,                       // CONTOSO\alice
		adMiss,                      // unmapped
	}

	stubData := buildLookupSids2StubData(sids)
	reqData := buildTestRequest(42, OpLsarLookupSids2, stubData)
	req, err := ParseRequest(reqData)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := h.HandleRequest(req)

	hdr, err := ParseHeader(response)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType == PDUFault {
		t.Fatal("got fault for a partially-unmapped batch; must be a normal response")
	}

	res := parseLookupSidsResponse(t, response)

	if res.status != statusSomeNotMapped {
		t.Errorf("status = 0x%08x, want STATUS_SOME_NOT_MAPPED (0x%08x)", res.status, statusSomeNotMapped)
	}
	if res.mappedCount != 3 {
		t.Errorf("mappedCount = %d, want 3", res.mappedCount)
	}
	if len(res.names) != 4 {
		t.Fatalf("translated names = %d, want 4", len(res.names))
	}

	// Per-SID types.
	wantTypes := []uint16{SidTypeAlias, SidTypeUser, SidTypeUser, SidTypeUnknown}
	for i, wt := range wantTypes {
		if res.names[i].sidType != wt {
			t.Errorf("name[%d].sidType = %d, want %d", i, res.names[i].sidType, wt)
		}
	}

	// The unmapped entry must carry domain index -1.
	if res.names[3].domainIdx != -1 {
		t.Errorf("unmapped name domainIdx = %d, want -1", res.names[3].domainIdx)
	}

	// Referenced domains: BUILTIN, FILER01, CONTOSO (in first-seen order).
	wantDomains := []string{"BUILTIN", "FILER01", "CONTOSO"}
	if len(res.domains) != len(wantDomains) {
		t.Fatalf("referenced domains = %v, want %v", res.domains, wantDomains)
	}
	for i, wd := range wantDomains {
		if res.domains[i] != wd {
			t.Errorf("domain[%d] = %q, want %q", i, res.domains[i], wd)
		}
	}

	// Each mapped name's domain index must point at the matching domain entry.
	checkIdx := func(nameIdx int, wantDomain string) {
		di := res.names[nameIdx].domainIdx
		if di < 0 || int(di) >= len(res.domains) {
			t.Errorf("name[%d] domainIdx %d out of range", nameIdx, di)
			return
		}
		if res.domains[di] != wantDomain {
			t.Errorf("name[%d] -> domain %q, want %q", nameIdx, res.domains[di], wantDomain)
		}
	}
	checkIdx(0, "BUILTIN")
	checkIdx(1, "FILER01")
	checkIdx(2, "CONTOSO")
}

// TestBuildLookupSidsResponse_TwoDomains is a focused round-trip over the
// referenced-domain list with exactly two distinct domains, verifying the
// dedup/index bookkeeping in isolation from the request parser.
func TestBuildLookupSidsResponse_TwoDomains(t *testing.T) {
	h := newTestLSAHandler()

	resolved := []resolvedSID{
		{name: "alice", sidType: SidTypeUser, domainName: "CONTOSO", domainSID: sid.ParseSIDMust("S-1-5-21-1-2-3")},
		{name: "bob", sidType: SidTypeUser, domainName: "FABRIKAM", domainSID: sid.ParseSIDMust("S-1-5-21-4-5-6")},
		{name: "carol", sidType: SidTypeUser, domainName: "CONTOSO", domainSID: sid.ParseSIDMust("S-1-5-21-1-2-3")},
	}

	domainMap := make(map[string]int)
	var domains []domainEntry
	for i := range resolved {
		r := &resolved[i]
		if _, ok := domainMap[r.domainName]; !ok {
			domainMap[r.domainName] = len(domains)
			domains = append(domains, domainEntry{name: r.domainName, sid: r.domainSID})
		}
	}

	stub := h.buildLookupSidsResponse(resolved, domains, domainMap, true)
	// Prefix a fake 24-byte RPC header so the shared parser offset math holds.
	response := append(make([]byte, 24), stub...)
	res := parseLookupSidsResponse(t, response)

	if len(res.domains) != 2 {
		t.Fatalf("domains = %v, want exactly 2 (deduped)", res.domains)
	}
	if res.domains[0] != "CONTOSO" || res.domains[1] != "FABRIKAM" {
		t.Errorf("domains = %v, want [CONTOSO FABRIKAM]", res.domains)
	}
	if res.names[0].domainIdx != 0 || res.names[1].domainIdx != 1 || res.names[2].domainIdx != 0 {
		t.Errorf("domain indices = [%d %d %d], want [0 1 0]",
			res.names[0].domainIdx, res.names[1].domainIdx, res.names[2].domainIdx)
	}
	if res.mappedCount != 3 {
		t.Errorf("mappedCount = %d, want 3", res.mappedCount)
	}
	if res.status != statusSuccess {
		t.Errorf("status = 0x%08x, want success", res.status)
	}
}
