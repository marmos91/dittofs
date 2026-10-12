package entity

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrDelay is a share paused by an administrator: the client retries.
	ErrDelay = errors.New("entity: share quiesced")
	// ErrStale is a handle whose file is gone, or into a share being removed
	// or retired: the client looks the name up again rather than create.
	ErrStale = errors.New("entity: stale handle")
	// ErrWrongShare is a handle of another share than the one asked.
	ErrWrongShare = errors.New("entity: handle of another share")
	// ErrInconsistent is an Nlink that would fall below zero: something else
	// still names the file, so the count was already wrong.
	ErrInconsistent = errors.New("entity: nlink underflow")
	// ErrCrossProject is a rename or link between two tree quotas.
	ErrCrossProject = errors.New("entity: rename or link across tree quotas")
)

// MaxShareName is the SMB share-name limit, in characters.
const MaxShareName = 80

// ValidShareName reports whether name may name a share: 1 to 80 characters
// of valid UTF-8, none of them a control character or one of \ / : * ? " < > |,
// and not IPC$.
func ValidShareName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxShareName {
		return false
	}
	if FoldShareName(name) == "IPC$" {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`\/:*?"<>|`, r) {
			return false
		}
	}
	return true
}

// HiddenShareName reports a name left out of share enumeration but still
// reachable by name.
func HiddenShareName(name string) bool { return strings.HasSuffix(name, "$") }

// ValidSharePath reports whether path is absolute and normalised: "/"
// followed by one or more components, each a valid file name, with no empty,
// "." or ".." component. "/" itself is not a share path: the pseudo-filesystem
// root is never a share.
func ValidSharePath(path string) bool {
	if len(path) < 2 || path[0] != '/' || !utf8.ValidString(path) {
		return false
	}
	for _, c := range strings.Split(path[1:], "/") {
		if c == "" || c == "." || c == ".." || len(c) > 255 || strings.ContainsRune(c, 0) {
			return false
		}
	}
	return true
}

// PathAncestor reports whether share path a is an ancestor of share path b.
// Both must be valid share paths.
func PathAncestor(a, b string) bool {
	return len(b) > len(a) && b[len(a)] == '/' && strings.HasPrefix(b, a)
}

// PathAncestors returns path's proper ancestors that could be share paths,
// nearest last: "/a/b/c" gives "/a", "/a/b".
func PathAncestors(path string) []string {
	var out []string
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			out = append(out, path[:i])
		}
	}
	return out
}

// Gate is the share state's answer to every call: nil when it admits,
// ErrDelay while quiesced, ErrStale while being removed or once retired. A
// cut's own gate holds writes, so a frozen share admits here. An unknown
// state refuses as stale rather than admit.
func (s ShareState) Gate() error {
	switch s {
	case ShareEnabled, ShareFrozen:
		return nil
	case ShareQuiesced:
		return ErrDelay
	default:
		return ErrStale
	}
}
