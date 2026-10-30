package platform

import (
	"reflect"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestSyscallTableIsReal pins every entry of the table NewOS uses on macOS to
// the golang.org/x/sys function the RFC's call goes through, by function
// pointer: a wrapper, such as a full-sync entry that calls unix.Fsync, or an
// entry pointing at another function, fails it. So does an entry added to the
// table without a pin here, a plain fsync entry among them.
func TestSyscallTableIsReal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := NewOS(t.TempDir()).calls
	pins := []struct {
		name, want string
		got, x     any
	}{
		{"open", "unix.Open", got.open, unix.Open},
		{"fcntl", "unix.FcntlInt", got.fcntl, unix.FcntlInt},
		{"fcntlFstore", "unix.FcntlFstore", got.fcntlFstore, unix.FcntlFstore},
		{"fstat", "unix.Fstat", got.fstat, unix.Fstat},
		{"ftruncate", "unix.Ftruncate", got.ftruncate, unix.Ftruncate},
		{"close", "unix.Close", got.close, unix.Close},
	}

	if n := reflect.TypeOf(calls{}).NumField(); n != len(pins) {
		t.Fatalf(
			"the table has %d entries and %d are pinned: the table has an entry nobody pinned",
			n,
			len(pins),
		)
	}

	for _, entry := range pins {
		if reflect.ValueOf(entry.got).Pointer() != reflect.ValueOf(entry.x).Pointer() {
			t.Errorf("the %s entry is not %s", entry.name, entry.want)
		}
	}
}
