package run

import (
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// killTree makes cancelling the command kill everything it started, and
// return only once all of it has exited, so none of it still holds a file or
// a directory when the command's Wait returns. Processes here have no groups:
// the tree is found from the parent each process records.
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// The command's own handle keeps its ID from being reused meanwhile.
		return cmd.Process.WithHandle(func(uintptr) { endTree(uint32(cmd.Process.Pid)) })
	}
}

// endTree ends a process, then its children, and theirs, each terminated and
// waited for before its children are looked for, so none can start a process
// that's missed. The handles it holds until it returns keep the ended
// processes' IDs from being reused. A recorded parent is only an ID, which a
// later process may have reused, so a process counts as a child only if it
// started no earlier than its parent.
func endTree(pid uint32) {
	started := map[uint32]int64{}
	var held []syscall.Handle
	end := func(pid uint32, after int64) {
		h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE, false, pid)
		if err != nil {
			return
		}
		var created, exited, kernel, user syscall.Filetime
		if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil || created.Nanoseconds() < after {
			_ = syscall.CloseHandle(h)
			return
		}
		_ = syscall.TerminateProcess(h, 1)
		_, _ = syscall.WaitForSingleObject(h, syscall.INFINITE)
		started[pid] = created.Nanoseconds()
		held = append(held, h)
	}
	end(pid, 0)
	// Look again after each pass that ended a process, until one ends none.
	for n := 0; n < len(held); {
		n = len(held)
		for child, parent := range parents() {
			if after, ok := started[parent]; ok {
				if _, ended := started[child]; !ended {
					end(child, after)
				}
			}
		}
	}
	for _, h := range held {
		_ = syscall.CloseHandle(h)
	}
}

// parents maps each process to the parent it records, from a snapshot of the
// process table.
func parents() map[uint32]uint32 {
	out := map[uint32]uint32{}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer func() { _ = syscall.CloseHandle(snap) }()
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		out[e.ProcessID] = e.ParentProcessID
	}
	return out
}

// killLeftovers does nothing: what a command leaves behind isn't tracked here.
func killLeftovers(*exec.Cmd) {}

// interruptTree keeps the default cancellation; nothing runs through sudo here.
func interruptTree(cmd *exec.Cmd, grace time.Duration) { cmd.WaitDelay = grace }
