package prof

import (
	"runtime"
	"strconv"
	"strings"
)

// ignored names frames of goroutines the standard library starts lazily, on
// first use, and keeps for the life of the process. A test that happens to be
// the first user would otherwise be blamed for them.
var ignored = []string{
	"os/signal.signal_recv",
	"os/signal.loop",
	"runtime.ensureSigM",
}

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
// in before, leaving out the ones the standard library starts for itself.
func leakedGoroutines(before map[uint64]string) []string {
	var leaked []string
	for id, stack := range goroutines() {
		if _, ok := before[id]; ok || isIgnored(stack) {
			continue
		}
		leaked = append(leaked, stack)
	}

	return leaked
}

func isIgnored(stack string) bool {
	for _, frame := range ignored {
		if strings.Contains(stack, frame) {
			return true
		}
	}

	return false
}
