package platform

import (
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"golang.org/x/sys/unix"
)

// TestSyscallTableIsReal pins every entry of the table NewOS uses to the
// golang.org/x/sys function the RFC's call goes through, by function pointer:
// a wrapper, or an entry pointing at another function, fails it. So does an
// entry added to the table without a pin here.
func TestSyscallTableIsReal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := NewOS(t.TempDir()).calls
	pins := []struct {
		name, want string
		got, x     any
	}{
		{"open", "unix.Open", got.open, unix.Open},
		{"fsync", "unix.Fsync", got.fsync, unix.Fsync},
		{"fdatasync", "unix.Fdatasync", got.fdatasync, unix.Fdatasync},
		{"close", "unix.Close", got.close, unix.Close},
	}

	if n := reflect.TypeOf(calls{}).NumField(); n != len(pins) {
		t.Fatalf("the table has %d entries and %d are pinned: the table has an entry nobody pinned", n, len(pins))
	}

	for _, entry := range pins {
		if reflect.ValueOf(entry.got).Pointer() != reflect.ValueOf(entry.x).Pointer() {
			t.Errorf("the %s entry is not %s", entry.name, entry.want)
		}
	}
}
