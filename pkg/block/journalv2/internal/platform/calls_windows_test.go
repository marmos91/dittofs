package platform

import (
	"reflect"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestSyscallTableIsReal pins every entry of the table NewOS uses to the
// golang.org/x/sys/windows function the RFC's call goes through, by function
// pointer: a wrapper, or an entry pointing at another function, fails it. So
// does an entry added to the table without a pin here, and an entry that is
// SetFileValidData under any name: it skips zero filling, so an allocated
// region would read whatever the disk held before (RFC 28 §2.2: "An allocated
// region no record has reached reads as zeros.").
func TestSyscallTableIsReal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := realTable
	pins := []struct {
		name, want string
		got, x     any
	}{
		{"createFile", "windows.CreateFile", got.createFile, windows.CreateFile},
		{
			"flushFileBuffers",
			"windows.FlushFileBuffers",
			got.flushFileBuffers,
			windows.FlushFileBuffers,
		},
		{"closeHandle", "windows.CloseHandle", got.closeHandle, windows.CloseHandle},
		{
			"getFileInformationByHandle",
			"windows.GetFileInformationByHandle",
			got.getFileInformationByHandle,
			windows.GetFileInformationByHandle,
		},
		{
			"setFileInformationByHandle",
			"windows.SetFileInformationByHandle",
			got.setFileInformationByHandle,
			windows.SetFileInformationByHandle,
		},
		{"writeFile", "windows.WriteFile", got.writeFile, windows.WriteFile},
	}

	table := reflect.ValueOf(got)

	if n := table.NumField(); n != len(pins) {
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

	forbidden := reflect.ValueOf(windows.SetFileValidData).Pointer()

	for i := range table.NumField() {
		if table.Field(i).Pointer() == forbidden {
			t.Errorf("the %s entry is windows.SetFileValidData", table.Type().Field(i).Name)
		}
	}
}
