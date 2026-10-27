package model

import (
	"slices"
	"testing"
)

// A seed draws the same sequence every time, and another seed another one, so
// a failing seed replays exactly.
func TestGenerateIsDecidedBySeed(t *testing.T) {
	a, b := Generate(1, 200), Generate(1, 200)
	if !slices.Equal(a, b) {
		t.Fatal("seed 1 drew two different sequences")
	}
	if slices.Equal(a, Generate(2, 200)) {
		t.Fatal("seeds 1 and 2 drew the same sequence")
	}
	if len(a) != 200 {
		t.Fatalf("drew %d steps, want 200", len(a))
	}
}

// Over a few seeds every operation, every crash, every share and every file is
// drawn, along with a call through the share that does not have the file and
// a write far out in the file, and every step drawn can be run.
func TestGenerateReachesEveryStep(t *testing.T) {
	ops, crashes := map[Op]bool{}, map[CrashKind]bool{}
	shares, files := map[int]bool{}, map[int]bool{}
	var otherShare, farOut bool
	for seed := range uint64(20) {
		for _, s := range Generate(seed, 200) {
			if why := s.valid(); why != "" {
				t.Fatalf("seed %d drew %+v: %s", seed, s, why)
			}
			ops[s.Op], shares[s.Share], files[s.File] = true, true, true
			if s.Op == OpCrash {
				k, _ := s.crash()
				crashes[k] = true
			}
			otherShare = otherShare || s.Share != s.File%numShares
			farOut = farOut || s.Op == OpWrite && s.Off > 1<<62
		}
	}
	for op := OpWrite; op < opEnd; op++ {
		if !ops[op] {
			t.Errorf("never drew %s", op)
		}
	}
	for _, k := range []CrashKind{CrashLost, CrashTorn, CrashReordered} {
		if !crashes[k] {
			t.Errorf("never drew a %s crash", k)
		}
	}
	if len(shares) != numShares || len(files) != numFiles || !otherShare || !farOut {
		t.Errorf("shares %v, files %v, through another share %t, far out %t", shares, files, otherShare, farOut)
	}
}
