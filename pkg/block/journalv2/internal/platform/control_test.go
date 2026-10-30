package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// controlFrame is the function an os.File's SyscallConn().Control runs its
// callback under. It is the os package's own name, not an exported one, so a
// Go release may rename it; TestInControl then fails, and so does every check
// that wants a call made inside the callback, rather than any of them passing
// on a call made outside it.
const controlFrame = "os.(*rawConn).Control"

// inControl reports whether its caller runs inside an os.File's
// SyscallConn().Control callback. The callback holds a reference on the
// file, so a Close made meanwhile cannot release the handle or descriptor,
// and the system cannot give its value to another file, until the callback
// returns.
func inControl() bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])

	for {
		fr, more := frames.Next()

		if fr.Function == controlFrame {
			return true
		}

		if !more {
			return false
		}
	}
}

// TestInControl checks inControl finds the Control frame from inside the
// callback and does not find it outside, so the table fakes that record it
// tell the two apart. A Go release that renames the frame fails it here.
func TestInControl(t *testing.T) {
	var f *os.File
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	f, err := os.Create(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}

	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	var inside bool

	if err := rc.Control(func(uintptr) {
		inside = inControl()
	}); err != nil {
		t.Fatal(err)
	}

	outside := inControl()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if !inside {
		t.Errorf("inControl inside the callback is false: no %s frame on the stack", controlFrame)
	}

	if outside {
		t.Errorf("inControl outside the callback is true")
	}
}
