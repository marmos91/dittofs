package prof

import (
	"runtime"
	"strconv"
	"strings"
)

// goroutines returns every live goroutine's stack, keyed by goroutine id.
func goroutines() map[uint64]string {
	out := make(map[uint64]string)
	for block := range strings.SplitSeq(string(dump()), "\n\n") {
		header, _, _ := strings.Cut(block, "\n")
		rest, ok := strings.CutPrefix(header, "goroutine ")
		if !ok {
			continue
		}

		idText, _, _ := strings.Cut(rest, " ")
		id, err := strconv.ParseUint(idText, 10, 64)
		if err != nil {
			continue
		}
		out[id] = block
	}

	return out
}

// dump returns every goroutine's stack. The buffer grows until the dump fits:
// runtime.Stack truncates rather than reporting how much it needed, and a
// truncated dump silently drops goroutines.
func dump() []byte {
	for size := 1 << 16; ; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return buf[:n]
		}
	}
}

// leakedGoroutines returns the stacks of the goroutines live now that were not
// in before.
//
// decision: no goroutine is exempt, not even the ones the standard library
// starts on first use and keeps for the life of the process. A test that is
// the first user of os/signal is blamed for its goroutines, and on macOS for
// its descriptors too, which no goroutine filter could excuse. Warm such a
// package up before the watch, as Watch does the poller, once a watched test
// needs one.
func leakedGoroutines(before map[uint64]string) []string {
	var leaked []string
	for id, stack := range goroutines() {
		if _, ok := before[id]; !ok {
			leaked = append(leaked, stack)
		}
	}

	return leaked
}
