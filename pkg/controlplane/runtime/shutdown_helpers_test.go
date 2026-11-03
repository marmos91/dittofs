package runtime

import (
	"runtime"
	"strings"
)

// goroutineDump returns every goroutine's stack. The buffer grows until the
// dump fits: runtime.Stack truncates rather than reporting how much it needed,
// and a truncated dump silently drops the frames the assertions look for.
func goroutineDump() string {
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return string(buf[:n])
		}
	}
}

// countGoroutines counts the live goroutines whose stack names frame. Naming a
// frame rather than counting every goroutine keeps the answer about the loop
// under test and not about whatever else the process happens to be running.
func countGoroutines(frame string) int {
	return strings.Count(goroutineDump(), frame)
}
