// Concurrency coverage for the mutable OpenFile fields the WRITE path touches.
//
// SMB clients legitimately pipeline operations on one FileID, and a
// multi-channel session dispatches them from different connections, so WRITE
// runs concurrently with the cleanup-time cache flush on the same handle. The
// test below is only meaningful under `go test -race`: it asserts nothing
// itself and passes trivially without the detector.
//
// The name/path/parent triple SET_INFO rename republishes is not covered the
// same way. Both sides of that pairing take the handle lock, which leaves the
// detector nothing to observe; the guard there is the lock itself.
package handlers

import (
	"sync"
	"testing"
)

// raceIterations is high enough for the detector to observe an interleaving on
// an unsynchronized field, low enough to stay a sub-second unit test.
const raceIterations = 200

// TestWrite_ConcurrentFlush_PayloadIDNoRace pairs the WRITE handler against
// flushFileCache, the cleanup-path reader of the cached payload. A file created
// empty has no payload until its first WRITE publishes the one the metadata
// store allocated, so the field is written on the data plane while session and
// tree teardown read it.
func TestWrite_ConcurrentFlush_PayloadIDNoRace(t *testing.T) {
	h, smbCtx, _, fileID := setupWriteTestShare(t, nil)
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("GetOpenFile: handle missing after setup")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for range raceIterations {
			h.flushFileCache(smbCtx.Context, openFile)
		}
	}()

	go func() {
		defer wg.Done()
		data := []byte("race")
		for range raceIterations {
			if _, err := h.Write(smbCtx, &WriteRequest{
				FileID: fileID,
				Length: uint32(len(data)),
				Data:   data,
			}); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
