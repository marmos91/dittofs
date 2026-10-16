package badgerkv

// syncDir does nothing on Windows.
//
// decision: Windows cannot flush a directory handle, and NTFS journals the
// entry of a file created in a directory, so the entry of a new memtable or
// value-log file is durable without one. A crash test on Windows that loses
// a new file's entry after its first acknowledged commit would overturn this.
func syncDir(string) error { return nil }
