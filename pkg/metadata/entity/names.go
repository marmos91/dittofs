package entity

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalidName is a name the share's name rule refuses.
	ErrInvalidName = errors.New("entity: invalid name")
	// ErrNameTooLong is a name longer than the share allows.
	ErrNameTooLong = errors.New("entity: name too long")
)

// NameRule decides which names a share accepts, for every protocol.
type NameRule uint8

const (
	// NamesPOSIX accepts every name a directory can hold: no separator, no
	// NUL, not "." or "..".
	NamesPOSIX NameRule = iota + 1
	// NamesWindows also refuses what a Windows client cannot open: invalid
	// UTF-8, the characters MS-FSCC forbids, a trailing space or dot, and a
	// reserved device name.
	NamesWindows
)

var nameRules = []string{NamesPOSIX: "posix", NamesWindows: "windows"}

func (r NameRule) String() string                { return enumString(nameRules, r) }
func (r NameRule) MarshalText() ([]byte, error)  { return enumMarshal(nameRules, r) }
func (r *NameRule) UnmarshalText(b []byte) error { return enumUnmarshal(nameRules, r, b) }

// ValidateName checks a name at the boundary, before anything stores or
// looks it up. It never rewrites the name: a name comes back as it went in.
func ValidateName(name []byte, rule NameRule, maxBytes int) error {
	if len(name) == 0 || bytes.IndexByte(name, '/') >= 0 || bytes.IndexByte(name, 0) >= 0 {
		return ErrInvalidName
	}
	if string(name) == "." || string(name) == ".." {
		return ErrInvalidName
	}
	if len(name) > maxBytes {
		return ErrNameTooLong
	}
	if rule != NamesWindows {
		return nil
	}
	if !utf8.Valid(name) || bytes.ContainsAny(name, `"*:<>?\|`) {
		return ErrInvalidName
	}
	for _, c := range name {
		if c < 0x20 {
			return ErrInvalidName
		}
	}
	if last := name[len(name)-1]; last == ' ' || last == '.' {
		return ErrInvalidName
	}
	if reservedDevice(string(name)) {
		return ErrInvalidName
	}
	return nil
}

// reservedDevice reports CON, PRN, AUX, NUL, COM0-9 and LPT0-9 in any case,
// alone or followed by a dot and anything.
func reservedDevice(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && base[3] >= '0' && base[3] <= '9' {
		switch strings.ToUpper(base[:3]) {
		case "COM", "LPT":
			return true
		}
	}
	return false
}

// Identity is the caller as this share sees it, resolved before it arrives:
// export policy and squashing are already applied. Every field a check may
// read is here, so a cache keyed by the identity must key on all of them.
type Identity struct {
	Principal Principal
	// Group is the group the request's credential names, resolved to its
	// principal: NFS's primary gid. Zero when the credential carries none,
	// as for SMB, or names no principal.
	Group Principal
	// Groups is every group Principal is in, nested ones included.
	Groups []Principal
	// Privileged marks a caller allowed what an owner is not: an unsquashed
	// root, or a principal the share grant names admin.
	Privileged bool
	// Authenticated marks a caller whose identity the service proved, as a
	// Kerberos or NTLM one is and an asserted AUTH_SYS uid is not: it matches
	// Authenticated Users and BUILTIN\Users.
	Authenticated bool
	// Admin marks a caller the share grant names admin: it matches
	// Administrators, whether or not it is privileged.
	Admin bool
}

// InGroup reports whether the caller is in g, through its credential group
// or its memberships.
func (id Identity) InGroup(g Principal) bool {
	if g.IsZero() {
		return false
	}
	if id.Group == g || id.Principal == g {
		return true
	}
	for _, m := range id.Groups {
		if m == g {
			return true
		}
	}
	return false
}
