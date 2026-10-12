package entity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// The upcase table is part of the store format: this hash pins it, so a
// regeneration under newer Unicode data fails here instead of changing the
// keys of every case-insensitive share.
func TestUpcaseTablePinned(t *testing.T) {
	h := sha256.New()
	for _, u := range upcasePairs {
		binary.Write(h, binary.BigEndian, u)
	}
	if got, want := hex.EncodeToString(h.Sum(nil)), upcaseTableHash; got != want {
		t.Fatalf("upcase table hash %s, want %s: a changed table is a new fold rule", got, want)
	}
}

const upcaseTableHash = "877290289bbd13e6832cc6b990a788f20ec0e60d092c6f51e50e357853a3c008"

// RFC 7 §12.1's case-insensitive name cases.
func TestFoldUpcase(t *testing.T) {
	same := func(a, b string) bool {
		return string(FoldUpcase.Fold([]byte(a))) == string(FoldUpcase.Fold([]byte(b)))
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"README", "readme", true},
		{"ReadMe.TXT", "README.txt", true},
		{"ǅ", "ǆ", true},
		{"Straße.docx", "STRASSE.docx", false},
		{"K.txt", "K.txt", false}, // the Kelvin sign upcases to itself
		{"ß", "ẞ", false},
		{"café", "café", false},            // composed and decomposed stay two
		{"\U00010428", "\U00010400", false}, // outside the BMP: compared as given
	} {
		if got := same(c.a, c.b); got != c.same {
			t.Errorf("%q vs %q: same %v, want %v", c.a, c.b, got, c.same)
		}
	}
	if got := FoldUpcase.Fold([]byte("a\xffb")); string(got) != "A\xffB" {
		t.Errorf("invalid UTF-8 folded to %q", got)
	}
	if got := FoldIdentity.Fold([]byte("ReadMe")); string(got) != "ReadMe" {
		t.Errorf("identity fold changed the name: %q", got)
	}
}

func TestDigest(t *testing.T) {
	var k1, k2 [DigestKeyLen]byte
	k2[0] = 1
	d := Digest(&k1, []byte("README"))
	if d < 3 || d>>63 != 0 {
		t.Fatalf("digest %#x out of range", d)
	}
	if Digest(&k1, []byte("README")) != d || Digest(&k2, []byte("README")) == d || Digest(&k1, []byte("README2")) == d {
		t.Error("digest is not a keyed function of the name")
	}
	if got, want := d, uint64(goldenDigest); got != want {
		t.Errorf("digest %#x, want %#x: the digest is part of the store format", got, want)
	}
}

const goldenDigest = 0x2ca3e6cfaac30e45
