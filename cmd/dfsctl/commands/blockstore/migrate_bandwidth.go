package blockstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/time/rate"
)

// ErrInvalidBandwidth marks a malformed --bandwidth-limit operand.
// Returned (wrapped) by ParseBandwidthLimit. D-A11.
var ErrInvalidBandwidth = errors.New("invalid --bandwidth-limit value")

// bandwidthSuffixRE captures the optional unit prefix (K/M/G/T/P), the
// optional 'i' that flips to 1024-base, and the optional trailing 'B'.
//
// The whole-string anchored shape and case-insensitive unit (`(?i)`)
// allow:
//
//	100               -> bytes
//	50KB / 50kb       -> 1000-base
//	50KiB / 50kib     -> 1024-base
//	1.5MB             -> fractional decimal allowed
//	-50MB             -> rejected (negative not allowed)
var bandwidthSuffixRE = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([KMGTP])?(I)?B?$`)

// ParseBandwidthLimit parses an operator-supplied --bandwidth-limit
// string and returns bytes-per-second.
//
// Suffix conventions (D-A11):
//
//	"" / "0"           → 0  (unlimited; Limiter is nil in caller)
//	"100"              → 100 B/s
//	"50KB" / "50kb"    → 50_000 B/s   (decimal, 1000-base)
//	"50KiB"            → 51_200 B/s   (binary, 1024-base)
//	"100MB" / "1GB"    → 1e8 / 1e9 B/s
//	"100MiB" / "1GiB"  → 1<<27 / 1<<30 B/s
//
// Negative values, unrecognized suffixes, and overflow return a non-nil
// error wrapping ErrInvalidBandwidth.
func ParseBandwidthLimit(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, nil
	}
	// Negative is fast-rejected before regex (the regex's `\d+` would
	// otherwise mis-anchor on `-50MB`).
	if strings.HasPrefix(trimmed, "-") {
		return 0, fmt.Errorf("%w: %q (negative not allowed)", ErrInvalidBandwidth, s)
	}
	// "0" with or without suffix: short-circuit to unlimited.
	// The regex handles bare "0" correctly anyway, but this avoids a
	// math.Floor call on the trivial path.
	if trimmed == "0" {
		return 0, nil
	}

	m := bandwidthSuffixRE.FindStringSubmatch(trimmed)
	if m == nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidBandwidth, s)
	}

	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q (parse: %v)", ErrInvalidBandwidth, s, err)
	}

	unit := strings.ToUpper(m[2])
	binary := m[3] != ""

	var multiplier float64 = 1
	switch unit {
	case "":
		// no prefix: pure bytes. A trailing 'i' or 'B' alone here would
		// have been captured by m[3]/the regex tail; "100B" with no
		// unit prefix maps to 100 bytes which is correct.
		if binary {
			// "100iB" or "100i" makes no sense (no unit prefix to scale).
			return 0, fmt.Errorf("%w: %q (i suffix requires unit prefix)", ErrInvalidBandwidth, s)
		}
		multiplier = 1
	case "K":
		if binary {
			multiplier = 1024
		} else {
			multiplier = 1000
		}
	case "M":
		if binary {
			multiplier = 1024 * 1024
		} else {
			multiplier = 1_000_000
		}
	case "G":
		if binary {
			multiplier = 1024 * 1024 * 1024
		} else {
			multiplier = 1_000_000_000
		}
	case "T":
		if binary {
			multiplier = 1024 * 1024 * 1024 * 1024
		} else {
			multiplier = 1_000_000_000_000
		}
	case "P":
		if binary {
			multiplier = 1024 * 1024 * 1024 * 1024 * 1024
		} else {
			multiplier = 1_000_000_000_000_000
		}
	default:
		return 0, fmt.Errorf("%w: %q (unknown unit %q)", ErrInvalidBandwidth, s, unit)
	}

	bps := math.Floor(val * multiplier)
	if bps < 0 || bps > float64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: %q (out of int64 range)", ErrInvalidBandwidth, s)
	}
	return int64(bps), nil
}

// minBurstFloor caps the smallest allowed token-bucket burst so that
// FastCDC's max chunk size (16 MiB) cannot exceed the burst size and
// trigger rate.Limiter.WaitN's "request greater than burst" rejection
// even at very low byte-rate ceilings. bandwidthWait splits oversize
// requests across multiple WaitN calls, so any sane floor works — 1 MiB
// strikes a balance between "small-enough that the first commit
// observes the configured limit" and "large-enough that single-chunk
// uploads don't fragment the WaitN call by default".
const minBurstFloor = 1 << 20 // 1 MiB

// newBandwidthLimiter returns a *rate.Limiter that allows bps
// bytes-per-second with a 1-second burst window (floored at 1 MiB to
// avoid pathologic burst-of-1 behavior).
//
// bps <= 0 → returns nil (= "unlimited"). Callers must nil-check
// before calling WaitN. bandwidthWait does so on their behalf.
func newBandwidthLimiter(bps int64) *rate.Limiter {
	if bps <= 0 {
		return nil
	}
	burst := int(bps)
	if int64(burst) < int64(minBurstFloor) {
		burst = minBurstFloor
	}
	return rate.NewLimiter(rate.Limit(bps), burst)
}

// bandwidthWait blocks until n bytes can be uploaded under the shared
// limiter. nil limiter = no waiting (returns nil immediately).
//
// rate.Limiter.WaitN refuses requests larger than its burst, so for
// huge chunks this helper splits the request across multiple WaitN
// calls. With burst floored at 1 MiB and FastCDC max chunk = 16 MiB,
// the split path activates whenever the configured byte-rate is below
// 16 MiB/s.
func bandwidthWait(ctx context.Context, l *rate.Limiter, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	burst := l.Burst()
	if burst <= 0 {
		// Defensive: if a future construction bug yields a zero-burst
		// limiter, treat as unlimited rather than spinning forever.
		return nil
	}
	for n > 0 {
		take := n
		if take > burst {
			take = burst
		}
		if err := l.WaitN(ctx, take); err != nil {
			return fmt.Errorf("bandwidthWait: %w", err)
		}
		n -= take
	}
	return nil
}
