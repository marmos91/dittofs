package prof

import "runtime"

// LiveHeap returns the bytes of heap still reachable. A collection runs
// first, so garbage made before the call does not count. A test measuring
// what a structure holds reads it before and after building the structure,
// and keeps the structure alive past the second read.
func LiveHeap() uint64 {
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return ms.HeapAlloc
}
