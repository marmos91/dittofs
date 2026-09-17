package commands

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	sharesvc "github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// The directive is the only thing an operator sees before the daemon exits, so
// it is checked like any other output: that it renders, that it says which
// direction the mismatch runs, and that the legacy arm names the release that
// can still migrate. Nothing covered it before, which is how its legacy arm
// went unreachable without anything failing.
func TestFormatMismatchDirective(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wants    []string
		notWants []string
	}{
		{
			name:     "state from a newer release",
			err:      fmt.Errorf("share %q: %w", "/srv/share", block.ErrFutureFormat),
			wants:    []string{"newer release", "Upgrades are one-way", "/srv/share"},
			notWants: []string{lastMigratingRelease, "pre-journal"},
		},
		{
			name:     "journal state from a newer release",
			err:      fmt.Errorf("share %q: %w", "/srv/share", journal.ErrFutureFormat),
			wants:    []string{"newer release"},
			notWants: []string{"pre-journal"},
		},
		{
			name: "pre-journal layout names the migrating release",
			err:  fmt.Errorf("share %q: %w", "/srv/share", sharesvc.ErrLegacyLocalFormat),
			// The operator needs three things: that nothing was destroyed, which
			// build converts the share, and that waiting will not help.
			wants: []string{
				"pre-journal", "Nothing on disk has been modified",
				lastMigratingRelease, "converts the share in place",
				"cannot be\nrecovered from the directory alone",
			},
			notWants: []string{"Upgrades are one-way"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatMismatchDirective(tc.err)
			// A mis-indexed verb renders as %!s(MISSING) or similar rather than
			// failing to compile, so the operator would get the artifact instead
			// of the version.
			if strings.Contains(got, "%!") {
				t.Fatalf("directive has a formatting artifact:\n%s", got)
			}
			for _, w := range tc.wants {
				if !strings.Contains(got, w) {
					t.Errorf("directive missing %q:\n%s", w, got)
				}
			}
			for _, w := range tc.notWants {
				if strings.Contains(got, w) {
					t.Errorf("directive should not contain %q:\n%s", w, got)
				}
			}
		})
	}
}

// The legacy arm is reachable only because handleFormatMismatch matches the
// sentinel. It previously did not, which left the arm dead and the share
// warn-and-skipped instead of stopping the boot.
//
// The guard now returns the exit status rather than calling exitFn itself, so
// the caller's defers (notably the control-plane store close) run before the
// process exits. The returned code is what is asserted here.
func TestHandleFormatMismatch_MatchesEverySentinel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"block future format", block.ErrFutureFormat, true},
		{"journal future format", journal.ErrFutureFormat, true},
		{"pre-journal layout", sharesvc.ErrLegacyLocalFormat, true},
		{"unrelated", errors.New("something else"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := handleFormatMismatch(tc.err, nil)
			if (got != nil) != tc.want {
				t.Fatalf("handleFormatMismatch = %v, want stop=%v", got, tc.want)
			}
			if got != nil && got.code != EX_CONFIG {
				t.Errorf("exit code = %d, want %d", got.code, EX_CONFIG)
			}
		})
	}
}
