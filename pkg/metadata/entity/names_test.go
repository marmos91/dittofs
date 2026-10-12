package entity

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	// RFC 7 §12.1's names check, plus the §3.2 rules every share applies.
	for _, c := range []struct {
		name           string
		posix, windows error
	}{
		{"readme", nil, nil},
		{"Straße.docx", nil, nil},
		{"CON", nil, ErrInvalidName},
		{"aux.txt", nil, ErrInvalidName},
		{"com7", nil, ErrInvalidName},
		{"LPT1.tar.gz", nil, ErrInvalidName},
		{"console", nil, nil},
		{"com10", nil, nil},
		{"a?b", nil, ErrInvalidName},
		{"dir.", nil, ErrInvalidName},
		{"trailing ", nil, ErrInvalidName},
		{"tab\there", nil, ErrInvalidName},
		{"\xff", nil, ErrInvalidName},
		{"", ErrInvalidName, ErrInvalidName},
		{".", ErrInvalidName, ErrInvalidName},
		{"..", ErrInvalidName, ErrInvalidName},
		{"a/b", ErrInvalidName, ErrInvalidName},
		{"a\x00b", ErrInvalidName, ErrInvalidName},
		{"...", nil, ErrInvalidName},
		{strings.Repeat("x", 256), ErrNameTooLong, ErrNameTooLong},
	} {
		if got := ValidateName([]byte(c.name), NamesPOSIX, 255); got != c.posix {
			t.Errorf("posix %q: %v, want %v", c.name, got, c.posix)
		}
		if got := ValidateName([]byte(c.name), NamesWindows, 255); got != c.windows {
			t.Errorf("windows %q: %v, want %v", c.name, got, c.windows)
		}
	}
}

func TestIdentityInGroup(t *testing.T) {
	g, cred, other := NewGroup(), NewGroup(), NewGroup()
	id := Identity{Principal: NewUser(), Group: cred, Groups: []Principal{g}}
	if !id.InGroup(g) || !id.InGroup(cred) || id.InGroup(other) || id.InGroup(Principal{}) {
		t.Error("InGroup")
	}
	if !id.InGroup(id.Principal) {
		t.Error("a principal is its own group of one")
	}
}
