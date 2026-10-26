//go:build !linux && !darwin

package prof

// descriptors returns false: this platform lists no descriptors through the
// standard library, so the descriptor check does not run.
func descriptors() (map[int]string, bool) {
	return nil, false
}
