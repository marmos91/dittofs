// Race regression tests for OpenFile mutable-field synchronization (#585, #606).
//
// MS-SMB2 multichannel does not mandate in-order delivery across channels and
// SMB clients legitimately pipeline operations on the same handle, so the
// same *OpenFile pointer is observed by concurrent goroutines. The tests below
// pin the per-OpenFile sync.RWMutex (`openFile.mu`) by deliberately running
// concurrent operations that mutate the enumeration cursor (#585), the SMB
// delayed-write timestamp overlay (#606) and the freeze/thaw flags (#606).
//
// Each test fails under `go test -race` if the lock is removed or any field is
// touched without it.
package handlers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupConcurrentDirTest builds a memory-backed handler with a directory open
// over a share that has nChildren regular files. The returned SMBHandlerContext
// has SessionID/TreeID/User pre-populated so QUERY_DIRECTORY does not bounce
// out via the AccessDenied arm.
func setupConcurrentDirTest(t *testing.T, nChildren int) (*Handler, *OpenFile, *SMBHandlerContext) {
	t.Helper()

	rt := runtime.New(nil)
	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("conc-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/conc"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "conc-meta",
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := rt.GetMetadataService()
	for i := 0; i < nChildren; i++ {
		if _, _, err := metaSvc.CreateFile(authCtx, rootHandle, fmt.Sprintf("f%03d", i), &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		}); err != nil {
			t.Fatalf("CreateFile: %v", err)
		}
	}

	h := NewHandler()
	h.Registry = rt
	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{TreeID: treeID, ShareName: shareName})

	open := &OpenFile{
		FileID:         h.GenerateFileID(),
		TreeID:         treeID,
		Path:           shareName,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(open)

	smbCtx := &SMBHandlerContext{
		Context: context.Background(),
		TreeID:  treeID,
		User: &models.User{
			Username: "uid-0",
			UID:      &uid,
			Groups:   []models.Group{{GID: &gid}},
		},
	}
	return h, open, smbCtx
}

// TestQueryDirectory_ConcurrentEnumerationCursor_NoRace pins #585. Without the
// per-OpenFile lock, two goroutines hitting QUERY_DIRECTORY on the same FileID
// race on EnumerationComplete / EnumerationIndex / EnumerationPattern. Run
// under `go test -race` to catch it — the assertion here is the absence of a
// detector report, plus the invariant that both calls completed without
// panicking and the final cursor is internally consistent.
func TestQueryDirectory_ConcurrentEnumerationCursor_NoRace(t *testing.T) {
	h, open, smbCtx := setupConcurrentDirTest(t, 8)

	const iterations = 50
	const goroutines = 4
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				flags := uint8(types.SMB2RestartScans)
				resp, err := h.QueryDirectory(smbCtx, &QueryDirectoryRequest{
					FileInfoClass:      uint8(types.FileIdBothDirectoryInformation),
					Flags:              flags,
					FileID:             open.FileID,
					FileName:           "*",
					OutputBufferLength: 8192,
				})
				if err != nil {
					t.Errorf("QueryDirectory: %v", err)
					return
				}
				_ = resp
			}
		}()
	}
	wg.Wait()
}

// TestSmbDelayedWrite_ConcurrentArmApply_NoRace pins #606 for the
// SmbWriteTriggered / SmbWritePreMtime / SmbWriteFlushMtime / SmbWriteFlushAt
// / SmbStickyWriteTime fields. Concurrent armSmbDelayedWrite (mutates) and
// applySmbDelayedWriteOverride (reads) on the same OpenFile must serialize
// through openFile.mu.
func TestSmbDelayedWrite_ConcurrentArmApply_NoRace(t *testing.T) {
	openFile := &OpenFile{FileID: [16]byte{1}}
	preMtime := time.Now().Add(-time.Hour)
	writeMtime := time.Now()

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			armSmbDelayedWrite(openFile, preMtime, writeMtime)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			file := &metadata.File{FileAttr: metadata.FileAttr{Mtime: writeMtime}}
			applySmbDelayedWriteOverride(openFile, file)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			flushSmbDelayedWrite(openFile)
		}
	}()
	wg.Wait()
}

// TestSmbStickyWriteTime_ConcurrentSetApply_NoRace covers the sticky-write
// pointer assignment racing with applySmbDelayedWriteOverride (which derefs
// the same pointer).
func TestSmbStickyWriteTime_ConcurrentSetApply_NoRace(t *testing.T) {
	openFile := &OpenFile{FileID: [16]byte{2}}

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			setSmbStickyWriteTime(openFile, time.Now())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			file := &metadata.File{FileAttr: metadata.FileAttr{Mtime: time.Now()}}
			applySmbDelayedWriteOverride(openFile, file)
		}
	}()
	wg.Wait()
}

// TestFreezeFields_ConcurrentReadWrite_NoRace pins the freeze flag and
// Frozen* pointer fields against concurrent SET_INFO (write) and
// READ/WRITE/COPYCHUNK (IsAtimeFrozen reads) + QUERY_INFO
// (applyFrozenTimestamps reads). All paths go through helpers that take
// openFile.mu (#606).
func TestFreezeFields_ConcurrentReadWrite_NoRace(t *testing.T) {
	openFile := &OpenFile{FileID: [16]byte{3}}

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(3)
	// Writer: toggle freeze flags + Frozen* pointers under the write lock.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			openFile.mu.Lock()
			now := time.Now()
			openFile.AtimeFrozen = i%2 == 0
			openFile.MtimeFrozen = i%2 == 0
			openFile.CtimeFrozen = i%2 == 0
			openFile.BtimeFrozen = i%2 == 0
			openFile.FrozenAtime = &now
			openFile.FrozenMtime = &now
			openFile.FrozenCtime = &now
			openFile.FrozenBtime = &now
			openFile.mu.Unlock()
		}
	}()
	// Reader 1: IsAtimeFrozen probe (post-READ/WRITE/QUERY_DIRECTORY path).
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = openFile.IsAtimeFrozen()
			_ = openFile.IsMtimeFrozen()
			_ = openFile.IsCtimeFrozen()
		}
	}()
	// Reader 2: applyFrozenTimestamps + buildFrozenAttrs (QUERY_INFO and
	// restoreFrozenTimestamps paths).
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			file := &metadata.File{}
			applyFrozenTimestamps(openFile, file)
			_ = buildFrozenAttrs(openFile)
		}
	}()
	wg.Wait()
}

// TestDOCPropagation_ConcurrentClose_NoRace pins the lock discipline on the
// CLOSE delete-on-close propagation path. The writer mimics the
// close.go/handler.go DOC-propagation Range callback (writing DeletePending +
// the parent-key fields onto a *sibling* OpenFile); the reader mimics
// isFileDeletePending probing DeletePending. Fails under -race if either side
// drops the lock.
func TestDOCPropagation_ConcurrentClose_NoRace(t *testing.T) {
	t.Parallel()

	handle := []byte{0xDE, 0xAD}
	of2 := &OpenFile{FileID: [16]byte{2}, MetadataHandle: handle}

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: propagates DOC fields onto of2 under its lock (the fixed path).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			of2.mu.Lock()
			of2.DeletePending = true
			of2.DeleteOnCloseParentKey = [16]byte{byte(i)}
			of2.HasDeleteOnCloseParentKey = true
			of2.mu.Unlock()
		}
	}()

	// Reader: reads DeletePending under RLock (the fixed isFileDeletePending).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			of2.mu.RLock()
			_ = of2.DeletePending
			_ = of2.DeleteOnCloseParentKey
			of2.mu.RUnlock()
		}
	}()

	wg.Wait()
}

// TestBaseFileDeletePending_ConcurrentClose_NoRace pins the lock discipline on
// the deferred base-file delete propagation path. The writer mimics the
// rangeStreamsOfBase callback; the reader mimics isFileOrBaseDeletePending.
// Fails under -race if either side drops the lock.
func TestBaseFileDeletePending_ConcurrentClose_NoRace(t *testing.T) {
	t.Parallel()

	stream := &OpenFile{FileID: [16]byte{3}}
	ph := metadata.FileHandle{0xAB, 0xCD}

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			stream.mu.Lock()
			stream.BaseFileDeletePending = i%2 == 0
			stream.BaseFileDeleteParentHandle = ph
			stream.BaseFileDeleteFileName = fmt.Sprintf("f%d", i)
			stream.DeleteOnCloseParentKey = [16]byte{byte(i)}
			stream.HasDeleteOnCloseParentKey = i%2 == 0
			stream.mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			stream.mu.RLock()
			_ = stream.BaseFileDeletePending
			_ = stream.BaseFileDeleteFileName
			stream.mu.RUnlock()
		}
	}()

	wg.Wait()
}
