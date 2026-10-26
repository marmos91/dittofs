package prof

import "runtime"

// liveHeap returns the bytes of heap still reachable. A collection runs first,
// so garbage made before the call does not count.
func liveHeap() uint64 {
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return ms.HeapAlloc
}
