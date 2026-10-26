package prof

import "runtime"

// liveHeap returns the bytes of heap still reachable. Two collections run
// first: objects with finalizers are freed only by the second.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return ms.HeapAlloc
}
