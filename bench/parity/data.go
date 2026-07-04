package parity

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// genChunk is the granularity of dataset generation and of engine WriteAt
// segments: 1 MiB, roughly a protocol-client write size and safely under the
// local store's per-record cap.
const genChunk = 1 << 20

// fileSeed derives the per-file RNG seed so dittofs and rclone datasets are
// byte-identical for the same (run seed, class, index) without staging the
// dittofs copy on disk.
func fileSeed(seed uint64, class string, index int) uint64 {
	h := seed ^ 0x9E3779B97F4A7C15
	for _, b := range []byte(class) {
		h = (h ^ uint64(b)) * 0x100000001B3
	}
	return (h ^ uint64(index)) * 0x100000001B3
}

// fillDeterministic fills buf with incompressible bytes from a PCG stream.
func fillDeterministic(rng *rand.PCG, buf []byte) {
	var w [8]byte
	for i := 0; i < len(buf); i += 8 {
		binary.LittleEndian.PutUint64(w[:], rng.Uint64())
		copy(buf[i:], w[:])
	}
}

// fileName returns the canonical dataset file name for an index.
func fileName(index int) string { return fmt.Sprintf("f%06d", index) }

// stageDataset materializes `count` files of `size` bytes under dir for the
// rclone lane. Idempotent: files already present with the right size are kept,
// so the dataset is staged once and reused across concurrency levels.
func stageDataset(dir string, class string, count int, size int64, seed uint64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	buf := make([]byte, genChunk)
	for i := 0; i < count; i++ {
		path := filepath.Join(dir, fileName(i))
		if st, err := os.Stat(path); err == nil && st.Size() == size {
			continue
		}
		rng := rand.NewPCG(fileSeed(seed, class, i), uint64(size))
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		for written := int64(0); written < size; {
			n := min(int64(len(buf)), size-written)
			fillDeterministic(rng, buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				_ = f.Close()
				return err
			}
			written += n
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
