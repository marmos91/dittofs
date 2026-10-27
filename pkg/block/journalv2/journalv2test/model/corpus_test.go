package model

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A sequence written as a corpus file reads back as the same steps, every
// field of every kind of step included.
func TestCorpusRoundTrips(t *testing.T) {
	steps := []Step{
		{Op: OpWrite, Share: 1, File: 3, Off: 1 << 40, Len: 4096, Byte: 0xfe, Pick: 7},
		{Op: OpSync},
		{Op: OpRead, File: 2},
		{Op: OpFill, File: 1, Pick: 255},
		{Op: OpStaleFill, Share: 1},
		{Op: OpOffload, Pick: 3, Off: 12, Len: 9, Byte: 'x'},
		{Op: OpRelease, Off: 5, Len: 2},
		{Op: OpTruncate, Off: 9},
		{Op: OpDeallocate, Off: 3, Len: 4},
		{Op: OpDelete, File: 1},
		{Op: OpSettle, Pick: 128},
		{Op: OpRepack},
		{Op: OpCrash, Pick: 2, Off: 1 << 62},
		{Op: OpReopen},
	}
	text := FormatCorpus(steps, "seed 7", "diverged at step 3")
	got, err := ParseCorpus(text)
	ok(t, err)
	if !slices.Equal(got, steps) {
		t.Fatalf("read back\n%s\nas %v, want %v", text, got, steps)
	}
	if !strings.Contains(string(text), "# seed 7\n") || !strings.Contains(string(text), "# diverged at step 3\n") {
		t.Fatalf("the notes are missing from\n%s", text)
	}
}

// A corpus line the reader does not fully understand is refused, never read
// as a different step.
func TestCorpusRefusesWhatItCannotRead(t *testing.T) {
	for _, line := range []string{
		"scribble",                            // no such operation
		"write f=1 f=2",                       // a field twice
		"write color=1",                       // no such field
		"write off=-1",                        // a negative offset
		"write len=x",                         // not a number
		"write s=2",                           // no such share
		"write f=4",                           // no such file
		"write b=256",                         // not a byte
		"write off=9223372036854775807 len=1", // past the largest offset
		"write off",                           // no value
	} {
		if _, err := ParseCorpus([]byte(line + "\n")); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
}

// The corpus of a directory is read in file-name order, so its replay order
// never depends on the filesystem's.
func TestCorpusIsReadInNameOrder(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct{ name, text string }{
		{"b.txt", "delete\n"},
		{"a.txt", "sync\n"},
		{"c.txt", "repack\n"},
		{"README", "not a sequence\n"},
	} {
		ok(t, os.WriteFile(filepath.Join(dir, f.name), []byte(f.text), 0o644))
	}
	got, err := ReadCorpus(dir)
	ok(t, err)
	var names []string
	var ops []Op
	for _, s := range got {
		names = append(names, s.Name)
		ops = append(ops, s.Steps[0].Op)
	}
	if !slices.Equal(names, []string{"a.txt", "b.txt", "c.txt"}) || !slices.Equal(ops, []Op{OpSync, OpDelete, OpRepack}) {
		t.Fatalf("read %v with %v", names, ops)
	}
}
