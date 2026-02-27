package sid

import (
	"bytes"
	"strings"
	"testing"
)

// ============================================================================
// SID Encode/Decode Tests (ported from security_test.go)
// ============================================================================

func TestSIDEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		sidStr string
	}{
		{"Everyone", "S-1-1-0"},
		{"CreatorOwner", "S-1-3-0"},
		{"CreatorGroup", "S-1-3-1"},
		{"NTAuthority", "S-1-5-18"},
		{"DomainUser1000", "S-1-5-21-100-200-300-3000"},
		{"DomainUser0", "S-1-5-21-100-200-300-1000"},
		{"Administrators", "S-1-5-32-544"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid, err := ParseSIDString(tt.sidStr)
			if err != nil {
				t.Fatalf("ParseSIDString(%q): %v", tt.sidStr, err)
			}

			var buf bytes.Buffer
			EncodeSID(&buf, sid)
			encoded := buf.Bytes()

			decoded, consumed, err := DecodeSID(encoded)
			if err != nil {
				t.Fatalf("DecodeSID: %v", err)
			}
			if consumed != len(encoded) {
				t.Errorf("DecodeSID consumed %d bytes, expected %d", consumed, len(encoded))
			}

			result := FormatSID(decoded)
			if result != tt.sidStr {
				t.Errorf("Round-trip failed: started %q, got %q", tt.sidStr, result)
			}
		})
	}
}

func TestSIDSize(t *testing.T) {
	// Everyone: S-1-1-0 (1 sub-authority) -> 8 + 4*1 = 12
	sid := ParseSIDMust("S-1-1-0")
	if got := SIDSize(sid); got != 12 {
		t.Errorf("SIDSize(S-1-1-0) = %d, want 12", got)
	}

	// Domain SID: 5 sub-authorities -> 8 + 4*5 = 28
	sid = ParseSIDMust("S-1-5-21-100-200-300-1000")
	if got := SIDSize(sid); got != 28 {
		t.Errorf("SIDSize(domain SID) = %d, want 28", got)
	}
}

func TestParseSIDStringErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"NoPrefix", "1-1-0"},
		{"TooShort", "S-1"},
		{"BadRevision", "S-abc-5"},
		{"BadAuthority", "S-1-abc"},
		{"BadSubAuthority", "S-1-5-abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSIDString(tt.input)
			if err == nil {
				t.Errorf("ParseSIDString(%q) should fail", tt.input)
			}
		})
	}
}

func TestDecodeSIDErrors(t *testing.T) {
	// Too short
	_, _, err := DecodeSID([]byte{1, 2, 3})
	if err == nil {
		t.Error("DecodeSID with 3 bytes should fail")
	}

	// SubAuthorityCount says 2 but not enough data
	data := []byte{1, 2, 0, 0, 0, 0, 0, 5}
	_, _, err = DecodeSID(data)
	if err == nil {
		t.Error("DecodeSID with insufficient sub-authority data should fail")
	}
}

func TestSIDEqual(t *testing.T) {
	a := ParseSIDMust("S-1-5-21-100-200-300-1000")
	b := ParseSIDMust("S-1-5-21-100-200-300-1000")
	c := ParseSIDMust("S-1-5-21-100-200-300-1001")

	if !a.Equal(b) {
		t.Error("Equal SIDs should be equal")
	}
	if a.Equal(c) {
		t.Error("Different SIDs should not be equal")
	}
	if a.Equal(nil) {
		t.Error("SID should not equal nil")
	}

	var nilSID *SID
	if nilSID.Equal(a) {
		t.Error("nil SID should not equal non-nil")
	}
}

// ============================================================================
// SIDMapper RID Mapping Tests
// ============================================================================

func TestUserSIDRIDMapping(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	tests := []struct {
		uid     uint32
		wantRID uint32
		wantSID string
	}{
		{1000, 3000, "S-1-5-21-100-200-300-3000"},
		{0, 0, "S-1-5-32-544"}, // root -> Administrators
		{1, 1002, "S-1-5-21-100-200-300-1002"},
		{500, 2000, "S-1-5-21-100-200-300-2000"},
	}

	for _, tt := range tests {
		t.Run(FormatSID(m.UserSID(tt.uid)), func(t *testing.T) {
			sid := m.UserSID(tt.uid)
			got := FormatSID(sid)
			if got != tt.wantSID {
				t.Errorf("UserSID(%d) = %s, want %s", tt.uid, got, tt.wantSID)
			}
		})
	}
}

func TestGroupSIDRIDMapping(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	tests := []struct {
		gid     uint32
		wantRID uint32
		wantSID string
	}{
		{1000, 3001, "S-1-5-21-100-200-300-3001"},
		{0, 1001, "S-1-5-21-100-200-300-1001"},
		{1, 1003, "S-1-5-21-100-200-300-1003"},
		{500, 2001, "S-1-5-21-100-200-300-2001"},
	}

	for _, tt := range tests {
		t.Run(FormatSID(m.GroupSID(tt.gid)), func(t *testing.T) {
			sid := m.GroupSID(tt.gid)
			got := FormatSID(sid)
			if got != tt.wantSID {
				t.Errorf("GroupSID(%d) = %s, want %s", tt.gid, got, tt.wantSID)
			}
		})
	}
}

func TestNoCollision(t *testing.T) {
	m := NewSIDMapper(111, 222, 333)

	// Verify UserSID(n) != GroupSID(n) for various n
	for _, n := range []uint32{0, 1, 100, 500, 1000, 5000, 65534} {
		userSID := m.UserSID(n)
		groupSID := m.GroupSID(n)

		if n == 0 {
			// UID 0 maps to Administrators, which is different from GroupSID(0)
			if userSID.Equal(groupSID) {
				t.Errorf("UserSID(0) should not equal GroupSID(0)")
			}
			continue
		}

		if userSID.Equal(groupSID) {
			t.Errorf("UserSID(%d) == GroupSID(%d): both %s", n, n, FormatSID(userSID))
		}
	}
}

func TestUIDFromSIDRoundTrip(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	for _, uid := range []uint32{1, 100, 500, 1000, 5000, 65534} {
		sid := m.UserSID(uid)
		got, ok := m.UIDFromSID(sid)
		if !ok {
			t.Errorf("UIDFromSID(UserSID(%d)) returned false", uid)
			continue
		}
		if got != uid {
			t.Errorf("UIDFromSID(UserSID(%d)) = %d", uid, got)
		}
	}
}

func TestGIDFromSIDRoundTrip(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	for _, gid := range []uint32{0, 1, 100, 500, 1000, 5000, 65534} {
		sid := m.GroupSID(gid)
		got, ok := m.GIDFromSID(sid)
		if !ok {
			t.Errorf("GIDFromSID(GroupSID(%d)) returned false", gid)
			continue
		}
		if got != gid {
			t.Errorf("GIDFromSID(GroupSID(%d)) = %d", gid, got)
		}
	}
}

func TestUIDFromSIDRejectsGroupSID(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	for _, id := range []uint32{0, 1, 100, 1000} {
		groupSID := m.GroupSID(id)
		if _, ok := m.UIDFromSID(groupSID); ok {
			t.Errorf("UIDFromSID(GroupSID(%d)) should return false", id)
		}
	}
}

func TestGIDFromSIDRejectsUserSID(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	for _, id := range []uint32{1, 100, 1000} {
		userSID := m.UserSID(id)
		if _, ok := m.GIDFromSID(userSID); ok {
			t.Errorf("GIDFromSID(UserSID(%d)) should return false", id)
		}
	}
}

func TestMachineSIDPersistence(t *testing.T) {
	// Generate -> serialize -> deserialize -> verify same mapping
	m1 := GenerateMachineSID()
	sidStr := m1.MachineSIDString()

	m2, err := NewSIDMapperFromString(sidStr)
	if err != nil {
		t.Fatalf("NewSIDMapperFromString(%q): %v", sidStr, err)
	}

	// Verify same UserSID for several UIDs
	for _, uid := range []uint32{1, 100, 1000, 65534} {
		s1 := m1.UserSID(uid)
		s2 := m2.UserSID(uid)
		if !s1.Equal(s2) {
			t.Errorf("After persistence, UserSID(%d) differs: %s vs %s",
				uid, FormatSID(s1), FormatSID(s2))
		}
	}

	// Verify same GroupSID
	for _, gid := range []uint32{0, 1, 100, 1000} {
		s1 := m1.GroupSID(gid)
		s2 := m2.GroupSID(gid)
		if !s1.Equal(s2) {
			t.Errorf("After persistence, GroupSID(%d) differs: %s vs %s",
				gid, FormatSID(s1), FormatSID(s2))
		}
	}
}

func TestMachineSIDStringFormat(t *testing.T) {
	m := NewSIDMapper(12345, 67890, 11111)
	got := m.MachineSIDString()
	want := "S-1-5-21-12345-67890-11111"
	if got != want {
		t.Errorf("MachineSIDString() = %q, want %q", got, want)
	}
}

func TestNewSIDMapperFromStringErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"InvalidFormat", "not-a-sid"},
		{"WrongAuthority", "S-1-1-0"},
		{"MissingSubAuth", "S-1-5-21-100-200"},
		{"NotDomain", "S-1-5-18"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSIDMapperFromString(tt.input)
			if err == nil {
				t.Errorf("NewSIDMapperFromString(%q) should fail", tt.input)
			}
		})
	}
}

func TestIsDomainSID(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	// Domain SID should match
	domainSID := m.UserSID(1000)
	if !m.IsDomainSID(domainSID) {
		t.Error("IsDomainSID should be true for domain user SID")
	}

	// Different machine SID should not match
	m2 := NewSIDMapper(999, 888, 777)
	otherSID := m2.UserSID(1000)
	if m.IsDomainSID(otherSID) {
		t.Error("IsDomainSID should be false for SID from different machine")
	}

	// Well-known SIDs should not match
	if m.IsDomainSID(WellKnownEveryone) {
		t.Error("IsDomainSID should be false for Everyone")
	}
	if m.IsDomainSID(WellKnownAdministrators) {
		t.Error("IsDomainSID should be false for Administrators")
	}

	// Nil should not match
	if m.IsDomainSID(nil) {
		t.Error("IsDomainSID should be false for nil")
	}
}

// ============================================================================
// PrincipalToSID / SIDToPrincipal Tests (ported from security_test.go)
// ============================================================================

func TestPrincipalToSID(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	tests := []struct {
		name       string
		who        string
		ownerUID   uint32
		ownerGID   uint32
		wantSID    string
		prefixOnly bool
	}{
		{"OwnerAt", "OWNER@", 1000, 1000, "S-1-5-21-100-200-300-3000", false},
		{"GroupAt", "GROUP@", 1000, 1001, "S-1-5-21-100-200-300-3003", false},
		{"EveryoneAt", "EVERYONE@", 0, 0, "S-1-1-0", false},
		{"NumericUID", "501@localdomain", 0, 0, "S-1-5-21-100-200-300-2002", false},
		{"NamedPrincipal", "alice@EXAMPLE.COM", 0, 0, "S-1-5-21-100-200-300-", true},
		{"RootOwner", "OWNER@", 0, 0, "S-1-5-32-544", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid := m.PrincipalToSID(tt.who, tt.ownerUID, tt.ownerGID)
			result := FormatSID(sid)

			if tt.prefixOnly {
				if !strings.HasPrefix(result, tt.wantSID) {
					t.Errorf("PrincipalToSID(%q) = %q, expected prefix %q", tt.who, result, tt.wantSID)
				}
			} else if result != tt.wantSID {
				t.Errorf("PrincipalToSID(%q) = %q, want %q", tt.who, result, tt.wantSID)
			}
		})
	}
}

func TestSIDToPrincipal(t *testing.T) {
	m := NewSIDMapper(100, 200, 300)

	tests := []struct {
		name      string
		sidStr    string
		wantPrinc string
	}{
		{"Everyone", "S-1-1-0", "EVERYONE@"},
		{"CreatorOwner", "S-1-3-0", "OWNER@"},
		{"CreatorGroup", "S-1-3-1", "GROUP@"},
		{"DomainUser1000", "S-1-5-21-100-200-300-3000", "1000@localdomain"},
		{"DomainGroup1000", "S-1-5-21-100-200-300-3001", "1000@localdomain"},
		{"UnknownSID", "S-1-5-32-544", "S-1-5-32-544"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid := ParseSIDMust(tt.sidStr)
			result := m.SIDToPrincipal(sid)
			if result != tt.wantPrinc {
				t.Errorf("SIDToPrincipal(%q) = %q, want %q", tt.sidStr, result, tt.wantPrinc)
			}
		})
	}
}

// ============================================================================
// Well-Known SID Tests
// ============================================================================

func TestAnonymousMapping(t *testing.T) {
	// Verify WellKnownAnonymous is S-1-5-7
	got := FormatSID(WellKnownAnonymous)
	if got != "S-1-5-7" {
		t.Errorf("WellKnownAnonymous = %s, want S-1-5-7", got)
	}
}

func TestWellKnownNames(t *testing.T) {
	tests := []struct {
		sid      *SID
		wantName string
		wantOK   bool
	}{
		{WellKnownEveryone, "Everyone", true},
		{WellKnownCreatorOwner, "CREATOR OWNER", true},
		{WellKnownCreatorGroup, "CREATOR GROUP", true},
		{WellKnownAnonymous, "NT AUTHORITY\\ANONYMOUS LOGON", true},
		{WellKnownSystem, "NT AUTHORITY\\SYSTEM", true},
		{WellKnownAdministrators, "BUILTIN\\Administrators", true},
		{WellKnownAuthenticatedUsers, "NT AUTHORITY\\Authenticated Users", true},
		{ParseSIDMust("S-1-5-21-0-0-0-1000"), "", false}, // Not well-known
	}

	for _, tt := range tests {
		name, ok := WellKnownName(tt.sid)
		if ok != tt.wantOK {
			t.Errorf("WellKnownName(%s): ok = %v, want %v", FormatSID(tt.sid), ok, tt.wantOK)
		}
		if name != tt.wantName {
			t.Errorf("WellKnownName(%s) = %q, want %q", FormatSID(tt.sid), name, tt.wantName)
		}
	}
}

func TestWellKnownSIDFormats(t *testing.T) {
	tests := []struct {
		sid  *SID
		want string
	}{
		{WellKnownEveryone, "S-1-1-0"},
		{WellKnownCreatorOwner, "S-1-3-0"},
		{WellKnownCreatorGroup, "S-1-3-1"},
		{WellKnownAnonymous, "S-1-5-7"},
		{WellKnownSystem, "S-1-5-18"},
		{WellKnownAdministrators, "S-1-5-32-544"},
		{WellKnownAuthenticatedUsers, "S-1-5-11"},
	}

	for _, tt := range tests {
		got := FormatSID(tt.sid)
		if got != tt.want {
			t.Errorf("FormatSID = %q, want %q", got, tt.want)
		}
	}
}
