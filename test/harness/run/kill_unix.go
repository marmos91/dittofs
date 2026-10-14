//go:build !windows

package run

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killTree makes cancelling the command kill everything it started: its own
// process group, and the group of each descendant that made one of its own,
// as timeout(1) does.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		for _, g := range groupsUnder(cmd.Process.Pid) {
			_ = syscall.Kill(-g, syscall.SIGKILL)
		}
		return nil
	}
}

// killLeftovers kills what an exited command left in its process group.
func killLeftovers(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// groupsUnder returns the process groups of pid and of its descendants, from
// a snapshot of the process table, leaving out dt's own.
func groupsUnder(pid int) []int {
	groups := []int{pid}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return groups
	}
	children := map[int][]int{}
	group := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		g, _ := strconv.Atoi(f[2])
		children[pp] = append(children[pp], p)
		group[p] = g
	}
	own := syscall.Getpgrp()
	seen := map[int]bool{pid: true, own: true}
	for queue := children[pid]; len(queue) > 0; queue = queue[1:] {
		p := queue[0]
		if g := group[p]; !seen[g] {
			seen[g] = true
			groups = append(groups, g)
		}
		queue = append(queue, children[p]...)
	}
	return groups
}

// interruptTree is killTree for a command whose tests run as root, which dt
// can't kill: cancelling sends SIGINT to every process group under it, which
// sudo passes on to the tests so their cleanup runs, and kills the command
// only if it hasn't ended within grace.
func interruptTree(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		for _, g := range groupsUnder(cmd.Process.Pid) {
			_ = syscall.Kill(-g, syscall.SIGINT)
		}
		return nil
	}
	cmd.WaitDelay = grace
}
