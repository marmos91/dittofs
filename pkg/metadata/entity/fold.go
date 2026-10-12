package entity

import (
	"encoding/binary"
	"unicode/utf8"

	"lukechampine.com/blake3"
)

//go:generate go run ./internal/upcasegen

// FoldRule is how a share's entry keys are folded, by the ID the store's
// format record names it by. A share's rule is bound when it is created and
// never changes, so no key is ever re-folded.
type FoldRule uint32

const (
	// FoldIdentity keys a name by its bytes: case-sensitive shares.
	FoldIdentity FoldRule = 1
	// FoldUpcase maps each UTF-16 code unit of a name through one frozen
	// 16-bit upcase table, unit for unit, with no normalisation: how NTFS
	// compares names. The table folds exactly what the Windows table folds,
	// so no two names Windows keeps apart become one entry. It is not Unicode
	// case folding: a letter outside the BMP is compared as given; ß, the
	// capital sharp s and the Kelvin, Ohm and Angstrom signs upcase to
	// themselves; and so do dotless ı, long ſ, final ς, the micro sign and
	// the titlecase digraphs such as ǅ.
	FoldUpcase FoldRule = 2
)

var foldRules = []string{FoldIdentity: "identity", FoldUpcase: "upcase"}

func (r FoldRule) String() string {
	if int(r) < len(foldRules) {
		return foldRules[r]
	}
	return ""
}

// FoldRules is every rule this binary implements.
var FoldRules = []FoldRule{FoldIdentity, FoldUpcase}

var upcase = func() *[1 << 16]uint16 {
	var t [1 << 16]uint16
	for i := range t {
		t[i] = uint16(i)
	}
	for i := 0; i < len(upcasePairs); i += 2 {
		t[upcasePairs[i]] = upcasePairs[i+1]
	}
	return &t
}()

// Fold is name's key under rule. Bytes that are not valid UTF-8 are kept as
// they are, so a name the posix rule admits still has one key.
func (r FoldRule) Fold(name []byte) []byte {
	if r != FoldUpcase {
		return name
	}
	out := make([]byte, 0, len(name))
	for len(name) > 0 {
		c, n := utf8.DecodeRune(name)
		switch {
		case c == utf8.RuneError && n <= 1:
			out = append(out, name[0])
		case c <= 0xFFFF:
			out = utf8.AppendRune(out, rune(upcase[c]))
		default:
			out = append(out, name[:n]...)
		}
		name = name[n:]
	}
	return out
}

// FoldShareName is the form share names are unique under: the upcase rule,
// as SMB servers compare them.
func FoldShareName(name string) string { return string(FoldUpcase.Fold([]byte(name))) }

// DigestKeyLen is the length of a share's digest key.
const DigestKeyLen = 32

// Digest orders a directory's entries and is the listing cookie after one:
// a 63-bit keyed hash of the entry's folded key, under the share's digest
// key. It is never 0, 1 or 2, the cookies NFS reserves, and its top bit is
// clear. The key is drawn at random when the share is created, so no client
// can choose names that collide.
func Digest(key *[DigestKeyLen]byte, folded []byte) uint64 {
	h := blake3.New(8, key[:])
	h.Write(folded)
	d := binary.BigEndian.Uint64(h.Sum(nil)) >> 1
	return max(d, 3)
}
