package suites

import (
	"os"
)

// openFileAppend opens a file for appending
func openFileAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
}
