package main

import (
	"testing"
	"unicode"
)

// The table is Unicode simple uppercase over the BMP, and never folds a unit
// Windows keeps apart: a pair it merged would lose a file on every copy from
// a Windows volume. It compares no Unicode version's output with the frozen
// table, which a newer Go may not reproduce and need not.
func TestPairs(t *testing.T) {
	ps := pairs()
	if len(ps) == 0 {
		t.Fatal("no mappings")
	}
	for _, p := range ps {
		if unicode.ToUpper(p[0]) != p[1] || p[0] == p[1] {
			t.Errorf("%U -> %U is not Unicode's simple uppercase", p[0], p[1])
		}
		if excluded(p[0]) || p[0] >= 0xD800 && p[0] <= 0xDFFF || p[0] > 0xFFFF || p[1] > 0xFFFF {
			t.Errorf("%U -> %U should be left out", p[0], p[1])
		}
	}
	for _, rg := range keptApart {
		if !excluded(rg[0]) {
			t.Errorf("%U is kept apart but not excluded", rg[0])
		}
	}
}
