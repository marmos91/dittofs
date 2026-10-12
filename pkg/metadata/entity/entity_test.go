package entity

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestIDTextRoundTrip(t *testing.T) {
	id := NewFileID()
	got, err := ParseFileID(id.String())
	if err != nil || got != id {
		t.Fatalf("ParseFileID(%s) = %v, %v", id, got, err)
	}
	for _, bad := range []string{"", "nope", "{" + id.String() + "}", "urn:uuid:" + id.String()} {
		if _, err := ParseShareID(bad); err == nil {
			t.Errorf("ParseShareID(%q) accepted", bad)
		}
	}
	if !(FileID{}).IsZero() || id.IsZero() {
		t.Error("IsZero")
	}
}

func TestPrincipalText(t *testing.T) {
	p := NewGroup()
	if s := p.String(); !strings.HasPrefix(s, "group:") {
		t.Fatalf("String() = %q", s)
	}
	b, err := json.Marshal(map[string]Principal{"p": p})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]Principal
	if err := json.Unmarshal(b, &back); err != nil || back["p"] != p {
		t.Fatalf("JSON round trip: %s -> %v, %v", b, back, err)
	}
	for _, bad := range []string{"", "user", "admin:" + p.ID.String(), "user:x"} {
		if _, err := ParsePrincipal(bad); err == nil {
			t.Errorf("ParsePrincipal(%q) accepted", bad)
		}
	}
	if _, err := (Principal{}).MarshalText(); err == nil {
		t.Error("the zero principal marshalled")
	}
}

func TestEnumText(t *testing.T) {
	for v := ShareEnabled; v <= ShareRetired; v++ {
		b, err := v.MarshalText()
		var back ShareState
		if err != nil || back.UnmarshalText(b) != nil || back != v {
			t.Errorf("%d: %q, %v -> %v", v, b, err, back)
		}
	}
	if s := ShareState(0).String(); s != "" {
		t.Errorf("unknown state printed as %q", s)
	}
	if _, err := ShareState(99).MarshalText(); err == nil {
		t.Error("unknown state marshalled")
	}
	var a ShareAccess
	if err := a.UnmarshalText([]byte("read-write")); err != nil || a != AccessReadWrite {
		t.Errorf("read-write -> %v, %v", a, err)
	}
}

func TestShareNames(t *testing.T) {
	for name, want := range map[string]bool{
		"photos":                true,
		"Back Up$":              true,
		"ünïcode":               true,
		strings.Repeat("é", 80): true,
		strings.Repeat("é", 81): false,
		"":                      false,
		"ipc$":                  false,
		"a/b":                   false,
		"a\\b":                  false,
		"a:b":                   false,
		"a*":                    false,
		"a?":                    false,
		`a"`:                    false,
		"a<":                    false,
		"a>":                    false,
		"a|":                    false,
		"a\tb":                  false,
		"a\x00":                 false,
		"\xff":                  false,
	} {
		if got := ValidShareName(name); got != want {
			t.Errorf("ValidShareName(%q) = %v", name, got)
		}
	}
	if FoldShareName("Photos") != FoldShareName("PHOTOS") || FoldShareName("a") == FoldShareName("b") {
		t.Error("fold")
	}
	if !HiddenShareName("admin$") || HiddenShareName("admin") {
		t.Error("hidden")
	}
}

func TestSharePaths(t *testing.T) {
	for path, want := range map[string]bool{
		"/photos":                      true,
		"/home/alice":                  true,
		"/":                            false,
		"":                             false,
		"photos":                       false,
		"/a/":                          false,
		"//a":                          false,
		"/a/./b":                       false,
		"/a/..":                        false,
		"/a\x00":                       false,
		"/" + strings.Repeat("x", 256): false,
	} {
		if got := ValidSharePath(path); got != want {
			t.Errorf("ValidSharePath(%q) = %v", path, got)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"/home", "/home/alice", true},
		{"/home", "/homes", false},
		{"/home", "/home", false},
		{"/home/alice", "/home", false},
	} {
		if got := PathAncestor(c.a, c.b); got != c.want {
			t.Errorf("PathAncestor(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if got := strings.Join(PathAncestors("/a/b/c"), ","); got != "/a,/a/b" {
		t.Errorf("PathAncestors = %s", got)
	}
}

func TestShareStateGate(t *testing.T) {
	for s, want := range map[ShareState]error{
		ShareEnabled: nil, ShareFrozen: nil, ShareQuiesced: ErrDelay,
		ShareRemoving: ErrStale, ShareRetired: ErrStale, 0: ErrStale,
	} {
		if got := s.Gate(); got != want {
			t.Errorf("%v.Gate() = %v, want %v", s, got, want)
		}
	}
}

func TestClientRuleValid(t *testing.T) {
	cidr := ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		r    ClientRule
		want bool
	}{
		{ClientRule{cidr, AccessRead}, true},
		{ClientRule{ClientMatch{Netgroup: "lab"}, AccessReadWrite}, true},
		{ClientRule{cidr, AccessAdmin}, false},
		{ClientRule{ClientMatch{}, AccessRead}, false},
		{ClientRule{ClientMatch{CIDR: cidr.CIDR, Netgroup: "lab"}, AccessRead}, false},
	} {
		if got := c.r.Valid(); got != c.want {
			t.Errorf("%+v.Valid() = %v", c.r, got)
		}
	}
}

// The fold is a 16-bit table: a character outside the BMP is not folded.
func TestFoldShareNameBMPOnly(t *testing.T) {
	if got := FoldShareName("ß𐐨é"); got != "ß𐐨É" {
		t.Errorf("FoldShareName = %q", got)
	}
}

func TestHandleOneSpelling(t *testing.T) {
	s, f := NewShareID(), NewFileID()
	h := NewHandle(s, f)
	gs, gf, err := h.Parse()
	if err != nil || gs != s || gf != f || h.Share() != s {
		t.Fatalf("Parse = %v %v %v", gs, gf, err)
	}
	for _, alias := range []Handle{h[:len(h)-1], append(append(Handle{}, h...), 0), append(Handle{2}, h[1:]...), nil} {
		if _, _, err := alias.Parse(); err != ErrStale {
			t.Errorf("alias %x parsed: %v", []byte(alias), err)
		}
	}
}
