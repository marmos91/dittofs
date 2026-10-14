package needs

import "errors"

// freeGB isn't measured here: no suite needs disk space on this platform.
func freeGB(string) (int, error) {
	return 0, errors.New("dt doesn't measure free disk here")
}
