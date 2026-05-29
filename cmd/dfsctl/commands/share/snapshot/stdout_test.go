package snapshot

import "os"

// osStdout / setStdout / restoreStdout let tests capture stdout written
// by fmt.Println / fmt.Printf without re-architecting the leaves to take
// an io.Writer.
func osStdout() *os.File { return os.Stdout }

func setStdout() (*os.File, *os.File) {
	r, w, _ := os.Pipe()
	os.Stdout = w
	return r, w
}

func restoreStdout(prev *os.File) {
	os.Stdout = prev
}

func osStderr() *os.File { return os.Stderr }
func osPipe() (*os.File, *os.File, error) {
	return os.Pipe()
}
func setStderr(w *os.File)        { os.Stderr = w }
func restoreStderr(prev *os.File) { os.Stderr = prev }
