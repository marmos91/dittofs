package bench

import (
	"fmt"
	"time"
)

// parseGoDuration wraps time.ParseDuration with a friendlier error message.
func parseGoDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid duration (e.g., 60s, 5m, 1h): %w", s, err)
	}
	return d, nil
}
