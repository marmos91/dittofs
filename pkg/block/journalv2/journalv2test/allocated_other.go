//go:build !unix

package journalv2test

import "errors"

// allocated is not implemented where the filesystem's allocation is not read
// through stat: the checks that need it run on unix only.
func allocated(dir string) (int64, error) {
	return 0, errors.New("allocated storage is read on unix only")
}
