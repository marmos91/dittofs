package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestLogFile_EofPos_InitialFresh verifies that getOrCreateLog initializes
// lf.eofPos to logHeaderSize on a fresh log (no prior on-disk file).
func TestLogFile_EofPos_InitialFresh(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	lf, _, _, err := bc.getOrCreateLog("file-fresh")
	if err != nil {
		t.Fatalf("getOrCreateLog: %v", err)
	}
	if lf.eofPos != logHeaderSize {
		t.Fatalf("fresh eofPos: got %d want %d", lf.eofPos, logHeaderSize)
	}
	path := bc.logPath("file-fresh")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if uint64(st.Size()) != lf.eofPos {
		t.Fatalf("fresh disk size %d != eofPos %d", st.Size(), lf.eofPos)
	}
}

// TestLogFile_EofPos_MatchesDiskSizeSequential writes several records back-
// to-back and asserts that lf.eofPos equals the actual on-disk size after
// each one. Locks the per-file mu before each read because the production
// path advances eofPos under that mutex.
func TestLogFile_EofPos_MatchesDiskSizeSequential(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	payload := bytes.Repeat([]byte{0x5A}, 256)
	for i, off := range []uint64{0, 4096, 8192, 12288, 16384} {
		if err := bc.AppendWrite(context.Background(), "file-seq", payload, off); err != nil {
			t.Fatalf("AppendWrite[%d]: %v", i, err)
		}
		path := bc.logPath("file-seq")
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		bc.logsMu.RLock()
		lf := bc.logFDs["file-seq"]
		mu := bc.logLocks["file-seq"]
		bc.logsMu.RUnlock()
		mu.Lock()
		got := lf.eofPos
		mu.Unlock()
		if uint64(st.Size()) != got {
			t.Fatalf("after write %d: disk size %d != eofPos %d", i, st.Size(), got)
		}
	}
}

// TestLogFile_EofPos_MatchesDiskSizeConcurrent fires N goroutines at the
// same payload and verifies the eofPos cursor stays consistent with the
// on-disk size when all writers have drained. The per-file mu serializes
// the increment, so the final value must equal the deterministic total
// of all framed-record sizes.
func TestLogFile_EofPos_MatchesDiskSizeConcurrent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	const goroutines = 32
	const payloadLen = 128
	payload := bytes.Repeat([]byte{0xAB}, payloadLen)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if err := bc.AppendWrite(context.Background(), "file-conc", payload, uint64(i*4096)); err != nil {
				t.Errorf("AppendWrite: %v", err)
			}
		}(i)
	}
	wg.Wait()

	path := bc.logPath("file-conc")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wantDisk := int64(logHeaderSize + goroutines*(recordFrameOverhead+payloadLen))
	if st.Size() != wantDisk {
		t.Fatalf("disk size: got %d want %d", st.Size(), wantDisk)
	}
	bc.logsMu.RLock()
	lf := bc.logFDs["file-conc"]
	mu := bc.logLocks["file-conc"]
	bc.logsMu.RUnlock()
	mu.Lock()
	got := lf.eofPos
	mu.Unlock()
	if uint64(st.Size()) != got {
		t.Fatalf("disk size %d != eofPos %d", st.Size(), got)
	}
}

// TestLogFile_EofPos_RestoredAfterRecover writes records, closes the
// FSStore, then reopens it via Recover. The reopened logFile must have
// eofPos restored to the on-disk size by recovery's seek(0, SeekEnd).
func TestLogFile_EofPos_RestoredAfterRecover(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	payload := bytes.Repeat([]byte{0x77}, 200)
	for _, off := range []uint64{0, 4096, 8192} {
		if err := bc.AppendWrite(context.Background(), "file-rec", payload, off); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
	}
	path := filepath.Join(bc.baseDir, "logs", "file-rec.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	preCloseSize := st.Size()

	bc2 := reopenFSStore(t, bc, rs)
	bc2.logsMu.RLock()
	lf := bc2.logFDs["file-rec"]
	mu := bc2.logLocks["file-rec"]
	bc2.logsMu.RUnlock()
	if lf == nil {
		t.Fatalf("logFile not restored after recover")
	}
	mu.Lock()
	got := lf.eofPos
	mu.Unlock()
	if uint64(preCloseSize) != got {
		t.Fatalf("post-recover eofPos: got %d want %d (on-disk size)", got, preCloseSize)
	}
}
