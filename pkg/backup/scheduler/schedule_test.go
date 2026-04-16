package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestValidateSchedule covers D-06 strict-at-write-time validation.
//
// Tests:
//   - T1: valid hourly cron passes
//   - T2: CRON_TZ prefix supported (robfig/cron/v3 ParseStandard)
//   - T3: gibberish wraps ErrScheduleInvalid
//   - T4: empty string rejected with ErrScheduleInvalid
//   - T5: every-minute "* * * * *" valid
func TestValidateSchedule(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		// Valid cases
		{"hourly", "0 * * * *", false},
		{"every minute", "* * * * *", false},
		{"daily at 3am", "0 3 * * *", false},
		{"every 5 minutes", "*/5 * * * *", false},
		{"cron_tz rome", "CRON_TZ=Europe/Rome 0 3 * * *", false},
		{"cron_tz new_york", "CRON_TZ=America/New_York 0 3 * * *", false},
		{"cron_tz utc", "CRON_TZ=UTC 30 2 * * *", false},

		// Invalid cases — must wrap ErrScheduleInvalid
		{"empty string", "", true},
		{"gibberish", "not a cron", true},
		{"too few fields", "0 *", true},
		{"out-of-range minute", "99 * * * *", true},
		{"out-of-range month", "0 0 * 13 *", true},
		{"unknown timezone", "CRON_TZ=Not/Real 0 3 * * *", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchedule(tc.expr)
			if tc.wantErr {
				require.Error(t, err)
				require.Truef(t, errors.Is(err, models.ErrScheduleInvalid),
					"error %q should wrap ErrScheduleInvalid", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestValidateSchedule_ErrorMessageContains ensures invalid schedules produce
// a helpful wrapped error message (useful in logs).
func TestValidateSchedule_ErrorMessageContains(t *testing.T) {
	err := ValidateSchedule("bogus")
	require.Error(t, err)
	// Error chain should match the sentinel AND the message should carry
	// something about the original failure.
	require.True(t, errors.Is(err, models.ErrScheduleInvalid))
	require.Contains(t, err.Error(), "invalid cron schedule expression")
}

// TestWrapScheduleError — internal helper retains identity when wrapping
// an already-wrapped sentinel.
func TestWrapScheduleError(t *testing.T) {
	// If err already wraps ErrScheduleInvalid, return as-is.
	original := ValidateSchedule("")
	require.Error(t, original)
	wrapped := wrapScheduleError("whatever", original)
	require.Same(t, original, wrapped, "should return original when it already wraps sentinel")

	// If err does NOT wrap ErrScheduleInvalid, wrap it.
	base := errors.New("some parse error")
	wrapped2 := wrapScheduleError("0 x x x x", base)
	require.True(t, errors.Is(wrapped2, models.ErrScheduleInvalid),
		"wrapped error should satisfy errors.Is(_, ErrScheduleInvalid)")
	require.Contains(t, wrapped2.Error(), "some parse error")
}
