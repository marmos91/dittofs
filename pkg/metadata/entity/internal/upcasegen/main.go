// Command upcasegen writes the frozen upcase table fold rule 2 names. It is
// run once: the table it writes is part of the store format, and a Go
// release with newer Unicode data must not change it. Rerunning it is a new
// fold rule, under a new ID.
//
// The table is Unicode simple uppercase over the BMP, less every unit the
// Windows upcase table leaves as itself, so it folds exactly the names
// Windows folds: a name pair Windows keeps apart is never merged into one
// entry, which would lose a file on every copy from a Windows volume. The
// Windows table compared against is the one NTFS formats with since Windows
// 7, as ntfs-3g rebuilds it; every mapping in it agrees with Unicode simple
// uppercase, so the difference is only the units below.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"log"
	"os"
	"unicode"
)

// keptApart are folds Unicode makes and Windows deliberately does not, among
// letters its table covers: compatibility and variant forms, final sigma,
// the titlecase digraphs, and the iota subscripts. Each stays a name of its
// own.
var keptApart = [][2]rune{
	{0x00B5, 0x00B5}, // micro sign
	{0x0131, 0x0131}, // latin small letter dotless i
	{0x017F, 0x017F}, // latin small letter long s
	{0x01C5, 0x01C5}, // latin capital letter d with small letter z with caron
	{0x01C8, 0x01C8}, // latin capital letter l with small letter j
	{0x01CB, 0x01CB}, // latin capital letter n with small letter j
	{0x01F2, 0x01F2}, // latin capital letter d with small letter z
	{0x0345, 0x0345}, // combining greek ypogegrammeni
	{0x03C2, 0x03C2}, // greek small letter final sigma
	{0x03D0, 0x03D1}, // greek beta symbol
	{0x03D5, 0x03D6}, // greek phi symbol
	{0x03F0, 0x03F1}, // greek kappa symbol
	{0x03F5, 0x03F5}, // greek lunate epsilon symbol
	{0x1E9B, 0x1E9B}, // latin small letter long s with dot above
	{0x1FBE, 0x1FBE}, // greek prosgegrammeni
}

// newerLetters are case pairs Unicode added, or gave a BMP capital, after the
// Windows table was built, which leaves them as they are. Folding them would
// merge names a Windows client keeps apart.
//
// decision: they stay unfolded even if a newer Windows table folds some.
// Keeping apart two names Windows would merge lets a Windows client see two
// entries it compares equal, which loses nothing; merging two names Windows
// keeps apart loses a file. Diff a current Windows table, under a new rule
// ID, if a client is shown to depend on one of these folds.
var newerLetters = [][2]rune{
	{0x019B, 0x019B}, // latin small letter lambda with stroke
	{0x023F, 0x0240}, // latin small letter s with swash tail
	{0x0252, 0x0252}, // latin small letter turned alpha
	{0x025C, 0x025C}, // latin small letter reversed open e
	{0x0261, 0x0261}, // latin small letter script g
	{0x0264, 0x0266}, // latin small letter rams horn
	{0x026A, 0x026A}, // latin letter small capital i
	{0x026C, 0x026C}, // latin small letter l with belt
	{0x0282, 0x0282}, // latin small letter s with hook
	{0x0287, 0x0287}, // latin small letter turned t
	{0x029D, 0x029E}, // latin small letter j with crossed-tail
	{0x03F3, 0x03F3}, // greek letter yot
	{0x0525, 0x0525}, // cyrillic small letter pe with descender
	{0x0527, 0x0527}, // cyrillic small letter shha with descender
	{0x0529, 0x0529}, // cyrillic small letter en with left hook
	{0x052B, 0x052B}, // cyrillic small letter dzzhe
	{0x052D, 0x052D}, // cyrillic small letter dche
	{0x052F, 0x052F}, // cyrillic small letter el with descender
	{0x10D0, 0x10FA}, // georgian letter an
	{0x10FD, 0x10FF}, // georgian letter aen
	{0x13F8, 0x13FD}, // cherokee small letter ye
	{0x1C80, 0x1C88}, // cyrillic small letter rounded ve
	{0x1C8A, 0x1C8A}, // cyrillic small letter tje
	{0x1D8E, 0x1D8E}, // latin small letter z with palatal hook
	{0x2C5F, 0x2C5F}, // glagolitic small letter caudate chrivi
	{0x2CEC, 0x2CEC}, // coptic small letter cryptogrammic shei
	{0x2CEE, 0x2CEE}, // coptic small letter cryptogrammic gangia
	{0x2CF3, 0x2CF3}, // coptic small letter bohairic khei
	{0x2D27, 0x2D27}, // georgian small letter yn
	{0x2D2D, 0x2D2D}, // georgian small letter aen
	{0xA661, 0xA661}, // cyrillic small letter reversed tse
	{0xA699, 0xA699}, // cyrillic small letter double o
	{0xA69B, 0xA69B}, // cyrillic small letter crossed o
	{0xA791, 0xA791}, // latin small letter n with descender
	{0xA793, 0xA794}, // latin small letter c with bar
	{0xA797, 0xA797}, // latin small letter b with flourish
	{0xA799, 0xA799}, // latin small letter f with stroke
	{0xA79B, 0xA79B}, // latin small letter volapuk ae
	{0xA79D, 0xA79D}, // latin small letter volapuk oe
	{0xA79F, 0xA79F}, // latin small letter volapuk ue
	{0xA7A1, 0xA7A1}, // latin small letter g with oblique stroke
	{0xA7A3, 0xA7A3}, // latin small letter k with oblique stroke
	{0xA7A5, 0xA7A5}, // latin small letter n with oblique stroke
	{0xA7A7, 0xA7A7}, // latin small letter r with oblique stroke
	{0xA7A9, 0xA7A9}, // latin small letter s with oblique stroke
	{0xA7B5, 0xA7B5}, // latin small letter beta
	{0xA7B7, 0xA7B7}, // latin small letter omega
	{0xA7B9, 0xA7B9}, // latin small letter u with stroke
	{0xA7BB, 0xA7BB}, // latin small letter glottal a
	{0xA7BD, 0xA7BD}, // latin small letter glottal i
	{0xA7BF, 0xA7BF}, // latin small letter glottal u
	{0xA7C1, 0xA7C1}, // latin small letter old polish o
	{0xA7C3, 0xA7C3}, // latin small letter anglicana w
	{0xA7C8, 0xA7C8}, // latin small letter d with short stroke overlay
	{0xA7CA, 0xA7CA}, // latin small letter s with short stroke overlay
	{0xA7CD, 0xA7CD}, // latin small letter s with diagonal stroke
	{0xA7CF, 0xA7CF}, // unassigned
	{0xA7D1, 0xA7D1}, // latin small letter closed insular g
	{0xA7D3, 0xA7D3}, // latin small letter double thorn
	{0xA7D5, 0xA7D5}, // latin small letter double wynn
	{0xA7D7, 0xA7D7}, // latin small letter middle scots s
	{0xA7D9, 0xA7D9}, // latin small letter sigmoid s
	{0xA7DB, 0xA7DB}, // latin small letter lambda
	{0xA7F6, 0xA7F6}, // latin small letter reversed half h
	{0xAB53, 0xAB53}, // latin small letter chi
	{0xAB70, 0xABBF}, // cherokee small letter a
}

func excluded(r rune) bool {
	for _, set := range [][][2]rune{keptApart, newerLetters} {
		for _, rg := range set {
			if r >= rg[0] && r <= rg[1] {
				return true
			}
		}
	}
	return false
}

func main() {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by internal/upcasegen from Unicode %s; DO NOT EDIT.\n\n", unicode.Version)
	b.WriteString("package entity\n\n")
	b.WriteString("// upcasePairs is fold rule 2's table: each BMP code unit Windows upcases,\n")
	b.WriteString("// followed by the unit it upcases to.\n")
	b.WriteString("var upcasePairs = [...]uint16{\n")
	n := 0
	for r := rune(0); r <= 0xFFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF || excluded(r) {
			continue // surrogates: a 16-bit table leaves them alone
		}
		if u := unicode.ToUpper(r); u != r && u <= 0xFFFF {
			fmt.Fprintf(&b, "0x%04X, 0x%04X,\n", r, u)
			n++
		}
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("upcase_table.go", src, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("%d mappings", n)
}
