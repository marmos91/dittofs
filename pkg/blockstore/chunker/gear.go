package chunker

// gearTable is the 256-entry FastCDC gear hash lookup table. Values are
// derived from a splitmix64-seeded PRNG (seed = 0x9E3779B97F4A7C15) so the
// table is deterministic across builds and platforms (D-21 boundary
// stability depends on this).
var gearTable = func() [256]uint64 {
	var t [256]uint64
	var state uint64 = 0x9E3779B97F4A7C15
	for i := 0; i < 256; i++ {
		// splitmix64
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z = z ^ (z >> 31)
		t[i] = z
	}
	return t
}()
