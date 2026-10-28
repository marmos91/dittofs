//go:build journal_conformance && unix

package conformance

import (
	"bytes"
	"fmt"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// The checks here lower the process's open-file limit, which is set through
// setrlimit on unix only.

// descriptorSlack is how many descriptors past the journal's bound the
// process may hold open while a journal runs.
//
// decision: the process's count also holds what the journal opens besides
// segment descriptors, its directory, its format file and the spares it is
// making, so it is held to the bound plus this, not to the bound alone. The
// ceiling: a journal holding up to this many segment descriptors past its
// bound passes the process's half of the check, and only Stats catches it.
// Overturned once Stats reports the descriptors it holds by kind.
const descriptorSlack = 8

// TestRFC1B_Descriptors checks that a journal with more segments than its
// descriptor bound reads every one of them, reopening the descriptors it
// closed, while the descriptors it holds stay within the bound by Stats and
// by the process's own count.
//
// A journal given no budget holds a quarter of the process's open-file limit,
// so the check lowers that limit to 64 while it runs: a bound of 16 against
// 40 segments at the fixture's 1 MiB cap.
func TestRFC1B_Descriptors(t *testing.T) {
	const fileLimit, size = 64, 256 << 10

	var prev syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &prev); err != nil {
		t.Fatalf("read the open-file limit: %v", err)
	}
	low := prev
	low.Cur = min(prev.Max, fileLimit)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatalf("lower the open-file limit: %v", err)
	}
	// Registered before the fixture, so the limit comes back only after its
	// journal has closed.
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &prev) })

	f := journalv2test.New(t)
	bound := f.J.Stats().DescriptorLimit
	if bound <= 0 || uint64(bound) > low.Cur/4 {
		t.Fatalf("DescriptorLimit = %d under an open-file limit of %d, want a positive bound of at most a quarter of it", bound, low.Cur)
	}
	base, counted := prof.Descriptors()
	if !counted {
		t.Log("the process's descriptors cannot be listed here; only Stats bounds them")
	}
	bounded := func(what string) {
		t.Helper()
		s := f.J.Stats()
		if s.OpenDescriptors > s.DescriptorLimit {
			t.Fatalf("%s: OpenDescriptors = %d, past DescriptorLimit %d", what, s.OpenDescriptors, s.DescriptorLimit)
		}
		if n, ok := prof.Descriptors(); counted && ok && n-base > bound+descriptorSlack {
			t.Fatalf("%s: the process holds %d descriptors more than once the journal opened, past its bound of %d and %d more", what, n-base, bound, descriptorSlack)
		}
	}

	var ws []written
	for i := int64(0); f.J.Stats().Segments <= 2*bound+8; i++ {
		if i*size > journalv2test.FixtureMaxBytes {
			t.Fatalf("%d segments after %d bytes written, want more than %d", f.J.Stats().Segments, i*size, 2*bound+8)
		}
		k := int(i % 2)
		id, off := journalv2test.File(k), i/2*size
		v := write(t, f.Share(k), id, off, pattern(fmt.Sprint("desc", i), size))
		ws = append(ws, written{k, id, journalv2.Extent{Off: off, Len: size}, v})
		bounded("while writing")
	}
	for _, k := range []int{0, 1} {
		syncNow(t, f.Share(k), journalv2test.File(k))
	}

	reopens := f.J.Stats().Counters.SegmentReopens
	for i, w := range ws {
		buf := make([]byte, size)
		_, missing, _, _, err := f.Share(w.share).ReadAt(t.Context(), w.id, w.extent.Off, buf)
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatalf("read write %d back with more segments than descriptors: %v", i, err)
		}
		if len(missing) != 0 || !bytes.Equal(buf, pattern(fmt.Sprint("desc", i), size)) {
			t.Fatalf("write %d reads back missing %+v or other bytes with more segments than descriptors", i, missing)
		}
		bounded("while reading")
	}
	if got := f.J.Stats().Counters.SegmentReopens; got <= reopens {
		t.Fatalf("SegmentReopens moved by %d over reads of %d segments under a bound of %d, want reopens counted", got-reopens, f.J.Stats().Segments, bound)
	}
}
