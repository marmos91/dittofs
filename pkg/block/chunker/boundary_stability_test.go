package chunker

import (
	"math/rand"
	"slices"
	"testing"
)

// Sizing small enough to run the whole cut-point sweep cheaply, large enough
// that a chunk crosses the small/large mask boundary at Avg and can reach the
// Max ceiling — so every branch of Next takes part.
var smallParams = Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}

// TestChunker_BoundaryIndependentOfBufferLength pins the property dedup rests
// on: where a chunk ends is decided by its bytes alone, never by how many bytes
// the caller happened to be holding when it asked.
//
// The packer accumulates into a buffer and asks Next where to cut, treating a
// zero answer as "not enough bytes yet". Each ask shows Next a different length
// of the same content. If the answer moved with that length, two writers of
// identical bytes would cut them differently — different chunk hashes, and dedup
// silently stops collapsing them across the fleet.
//
// So: for one chunk start, sweep the buffer length across every interesting
// point and require that every non-zero boundary reported is the same one.
func TestChunker_BoundaryIndependentOfBufferLength(t *testing.T) {
	data := randomBlob(t, 1<<20, 7)

	c := NewChunkerWithParams(smallParams)
	chunks := 0
	for pos := 0; pos < len(data); {
		rest := data[pos:]

		// The boundary the packer would settle on: the buffer is filled to its
		// cap before each ask, so this is the answer production actually takes.
		final := len(rest) <= smallParams.Max
		want, _ := c.Next(rest, final)
		if want == 0 {
			t.Fatalf("pos %d: Next withheld a boundary on a %d-byte buffer", pos, len(rest))
		}

		for _, n := range sweepLengths(len(rest), smallParams) {
			// final only when this really is the last of the content, which is
			// what the packer knows from its reader hitting EOF.
			got, _ := c.Next(rest[:n], n == len(rest) && final)
			if got == 0 {
				continue // Next is entitled to ask for more bytes; it is not entitled to change its mind
			}
			if got != want {
				t.Fatalf("pos %d: buffer length %d yields boundary %d, but %d yields %d — "+
					"the cut moved with the buffer, not the content",
					pos, n, got, len(rest), want)
			}
		}

		pos += want
		chunks++
	}
	if chunks < 8 {
		t.Fatalf("blob cut into %d chunks, too few for the sweep to be evidence", chunks)
	}
}

// TestChunker_BoundaryIndependentOfBufferLength_DefaultParams runs the same
// sweep against the shipped 1M/4M/16M profile. Every existing share chunks with
// it, so it is the profile whose boundaries must not drift; the small-params
// case exercises the branches, this one guards the bytes already on disk.
func TestChunker_BoundaryIndependentOfBufferLength_DefaultParams(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-MiB default-profile sweep under -short")
	}
	p := DefaultParams()
	data := randomBlob(t, 5<<20, 11)

	c := NewChunkerWithParams(p)
	chunks := 0
	for pos := 0; pos < len(data); {
		rest := data[pos:]
		final := len(rest) <= p.Max
		want, _ := c.Next(rest, final)
		if want == 0 {
			t.Fatalf("pos %d: Next withheld a boundary on a %d-byte buffer", pos, len(rest))
		}
		for _, n := range sweepLengths(len(rest), p) {
			got, _ := c.Next(rest[:n], n == len(rest) && final)
			if got != 0 && got != want {
				t.Fatalf("pos %d: buffer length %d yields boundary %d, but %d yields %d",
					pos, n, got, len(rest), want)
			}
		}
		pos += want
		chunks++
	}
	if chunks < 4 {
		t.Fatalf("blob cut into %d chunks, too few for the sweep to be evidence", chunks)
	}
}

// sweepLengths returns the buffer lengths worth showing Next for content of
// length total. It pins the points where Next changes behaviour — the Min floor
// below which it withholds an answer, the Avg mask switch, and the Max ceiling
// where it cuts regardless of content — since those are where a length-sensitive
// bug surfaces, then spreads a bounded number of samples across the rest. The
// count is bounded rather than the stride because each sample costs a scan of
// the whole buffer, so a fixed stride over the default profile's megabytes is
// minutes of work for no more evidence.
func sweepLengths(total int, p Params) []int {
	const samples = 40
	stride := max((min(total, p.Max)-p.Min)/samples, 1)
	seen := map[int]struct{}{}
	var out []int
	add := func(n int) {
		if n <= 0 || n > total {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range []int{p.Min - 1, p.Min, p.Min + 1, p.Avg - 1, p.Avg, p.Avg + 1, p.Max - 1, p.Max, total} {
		add(n)
	}
	for n := p.Min; n < min(total, p.Max); n += stride {
		add(n)
	}
	slices.Sort(out)
	return out
}

// TestChunker_StreamedCutsIndependentOfBufferCap chunks a whole blob the way the
// packer does — fill, cut, carry the remainder — and requires the resulting cut
// points to be identical whatever size buffer the packer accumulates into. The
// packer's buffer is a performance choice; this is what keeps it from being a
// format choice as well.
func TestChunker_StreamedCutsIndependentOfBufferCap(t *testing.T) {
	data := randomBlob(t, 1<<20, 23)

	// A cap below Max would leave Next asking for bytes the caller can no longer
	// supply (see Params.Validate), so the sweep starts at Max.
	want := chunkStream(t, smallParams, data, smallParams.Max)
	if len(want) < 8 {
		t.Fatalf("blob cut into %d chunks, too few for the comparison to be evidence", len(want))
	}
	for _, bufCap := range []int{smallParams.Max + 1, 20 << 10, 64 << 10, 1 << 20, MaxChunkSize} {
		got := chunkStream(t, smallParams, data, bufCap)
		if !slices.Equal(got, want) {
			t.Errorf("bufCap=%d: cut points differ from a %d-byte buffer\n got %v\nwant %v",
				bufCap, smallParams.Max, got, want)
		}
	}
}

// TestChunker_StreamedCutsIndependentOfBufferCap_Compressible repeats the sweep
// over content with few natural breakpoints, where chunks are cut by the Max
// ceiling rather than by the fingerprint. That leaves Next through a different
// return, and it is the shape files full of zeroes or repeated structure take.
func TestChunker_StreamedCutsIndependentOfBufferCap_Compressible(t *testing.T) {
	data := make([]byte, (1<<20)+1237)
	rng := rand.New(rand.NewSource(13))
	for i := 0; i < len(data); i += 64 << 10 {
		data[i] = byte(rng.Intn(256))
	}

	want := chunkStream(t, smallParams, data, smallParams.Max)
	for _, bufCap := range []int{smallParams.Max + 1, 20 << 10, 64 << 10, 1 << 20} {
		got := chunkStream(t, smallParams, data, bufCap)
		if !slices.Equal(got, want) {
			t.Errorf("bufCap=%d: cut points differ from a %d-byte buffer\n got %v\nwant %v",
				bufCap, smallParams.Max, got, want)
		}
	}
}

// chunkStream reproduces the accumulate-and-cut loop the carve packer runs over
// a dirty run — fill a buffer to bufCap, ask Next where to cut, read more on a
// zero answer, carry the remainder — and returns the chunk end offsets in the
// blob's own frame. Those offsets are what decide content hashes.
func chunkStream(t testing.TB, p Params, data []byte, bufCap int) []int {
	t.Helper()

	c := NewChunkerWithParams(p)
	buf := make([]byte, 0, bufCap)
	var ends []int
	pos, consumed := 0, 0
	eof := false

	for {
		if n := min(cap(buf)-len(buf), len(data)-pos); n > 0 {
			buf = append(buf, data[pos:pos+n]...)
			pos += n
		}
		eof = pos == len(data)
		if len(buf) == 0 {
			break
		}
		boundary, _ := c.Next(buf, eof)
		if boundary == 0 {
			// Next may only withhold a boundary while it can still be handed more
			// bytes. A full buffer with nothing left to cut would spin here, so
			// fail loudly rather than hang the package.
			if len(buf) == cap(buf) || eof {
				t.Fatalf("no progress: Next returned 0 on a %d/%d-byte buffer (eof=%v, params %+v)",
					len(buf), cap(buf), eof, p)
			}
			continue
		}
		consumed += boundary
		ends = append(ends, consumed)
		buf = append(buf[:0], buf[boundary:]...)
		if eof && len(buf) == 0 {
			break
		}
	}
	return ends
}

func randomBlob(t testing.TB, size int, seed int64) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.New(rand.NewSource(seed)).Read(b); err != nil {
		t.Fatalf("generate blob: %v", err)
	}
	return b
}
