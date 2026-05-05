//go:build linux || darwin

package engine

import (
	"fmt"
	"os"
	"syscall"
)

// mmapThresholdBytes is the file-size cutoff below which readFromCAS
// uses os.ReadFile instead of mmap. Mmap setup overhead (mmap +
// page-fault + munmap) dominates for tiny chunks; the FastCDC chunker
// (Phase 10) targets min=1 MiB / avg=4 MiB so almost every CAS object
// is well above this threshold. The fallback is for the remainder
// (legacy small files, sparse-block tails, fixture data in tests).
//
// 64 KiB matches the Phase 12 D-33 planner discretion call documented
// in 12-CONTEXT.md.
const mmapThresholdBytes = 64 * 1024

// readFromCAS reads up to len(dest) bytes from path, starting at the
// given byte offset, into dest. It returns the number of bytes copied
// (which may be less than len(dest) if the file ends first or the
// offset is beyond EOF).
//
// On Linux/Darwin, files >= mmapThresholdBytes are mmap'd PROT_READ +
// MAP_SHARED — pages flow from the kernel page cache directly into
// dest via a single copy. Smaller files use os.ReadFile (mmap setup
// overhead dominates for tiny reads, planner discretion D-33).
//
// True zero-copy via an mmap-to-caller []byte view is intentionally
// rejected (D-33): caller-held views into mmap'd memory create lifetime
// hazards (SIGSEGV on early munmap, racy munmap during caller use).
// The single-copy contract — copy(dest, mapped[off:]) then immediate
// munmap — is the safe upper bound.
//
// Threats covered:
//
//   - T-12-27 (file modified during read): MAP_SHARED + PROT_READ; CAS
//     files are immutable per Phase 11 INV-01 + LSL-02 atomic .tmp+rename.
//   - T-12-28 (mmap descriptor leak): defer Munmap + defer Close on every
//     return path.
//   - T-12-29 (information disclosure): copy bounded by len(dest); mapped
//     pages unmapped before return; caller never sees the mmap region.
func readFromCAS(path string, offset uint32, dest []byte) (int, error) {
	if len(dest) == 0 {
		return 0, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open cas chunk %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat cas chunk %s: %w", path, err)
	}
	sz := fi.Size()
	if int64(offset) >= sz {
		return 0, nil // offset past EOF — nothing to copy
	}

	if sz < mmapThresholdBytes {
		// Below mmap threshold — ReadFile fallback per D-33.
		buf := make([]byte, sz)
		if _, err := f.ReadAt(buf, 0); err != nil {
			return 0, fmt.Errorf("readfile cas chunk %s: %w", path, err)
		}
		return copy(dest, buf[offset:]), nil
	}

	mapped, err := syscall.Mmap(int(f.Fd()), 0, int(sz),
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mmap cas chunk %s: %w", path, err)
	}
	defer func() {
		// Best-effort munmap; if it fails the kernel will reclaim on
		// process exit. We do NOT propagate the error because the copy
		// above already completed successfully — the read is correct.
		_ = syscall.Munmap(mapped)
	}()

	return copy(dest, mapped[offset:]), nil
}
