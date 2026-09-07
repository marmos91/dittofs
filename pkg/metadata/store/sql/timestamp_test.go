package sql

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// The advertised resolution has to be the one the row codec actually keeps.
// Too fine and the backends promise clients a precision they discard; too
// coarse and they understate what they can hold.
//
// The expectation is measured out of the codec rather than derived from the
// constant, so a wrong constant cannot agree with it by construction.
func TestTimestampResolutionMatchesCodec(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()

	survives := func(d time.Duration) bool {
		stepped := base.Add(d)
		return sqlcodec.FiletimeToTime(sqlcodec.TimeToFiletime(stepped)).Equal(stepped)
	}

	// The smallest positive step the round trip preserves.
	var granularity time.Duration
	for d := time.Nanosecond; d <= time.Microsecond; d += time.Nanosecond {
		if survives(d) {
			granularity = d
			break
		}
	}
	if granularity == 0 {
		t.Fatal("no step up to 1µs survived the round trip; the codec is coarser than this test can measure")
	}

	if TimestampResolution != granularity {
		t.Errorf("TimestampResolution is %v but the codec keeps %v", TimestampResolution, granularity)
	}
}
