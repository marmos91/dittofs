package run

import (
	"bytes"
	"testing"
)

// A non-terminal writer (pipe, file, CI) must read as non-interactive so
// progress stays sparse and fio's per-percent ETA is suppressed.
func TestIsInteractive_NonTTY(t *testing.T) {
	if isInteractive(&bytes.Buffer{}) {
		t.Fatal("a bytes.Buffer is not a terminal")
	}
}
