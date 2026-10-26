//go:build linux

package journalv2test

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

// TestPartialWriteIsUndoneByACrash writes past a file-size limit, so the
// write lands in part and then fails. os.File.WriteAt reports n == 0 for it,
// yet the landed bytes are on the file, and a crash must still undo them. The
// limit applies to the whole process, so the test runs itself in a child.
func TestPartialWriteIsUndoneByACrash(t *testing.T) {
	if os.Getenv("SEAM_PARTIAL_WRITE_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPartialWriteIsUndoneByACrash$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SEAM_PARTIAL_WRITE_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}

	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "ab", 0)
	syncFile(t, f)

	// Only the soft limit is lowered, and it is restored right after the write:
	// a non-root process cannot raise the hard limit back, and anything the
	// child writes later, such as its coverage profile at exit, needs the room.
	signal.Ignore(syscall.SIGXFSZ)
	var prev syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &prev); err != nil {
		t.Fatalf("read RLIMIT_FSIZE: %v", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 4, Max: prev.Max}); err != nil {
		t.Skipf("cannot lower RLIMIT_FSIZE: %v", err)
	}

	n, err := f.WriteAt([]byte("cdef"), 2)
	if rerr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &prev); rerr != nil {
		t.Fatalf("restore RLIMIT_FSIZE: %v", rerr)
	}
	if err == nil {
		t.Fatal("a write past the file-size limit succeeded")
	}
	if got := onDisk(t, s, "seg"); got != "abcd" {
		t.Fatalf("before the crash the file holds %q (n = %d), want the partial %q", got, n, "abcd")
	}

	crash(t, s)

	if got := onDisk(t, s, "seg"); got != "ab" {
		t.Fatalf("after the crash the file holds %q, want the synced %q", got, "ab")
	}
}
