package commands

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block"
)

// The directive is the only thing an operator sees before the daemon exits, so
// it is checked like any other output: that it renders and names the store.
func TestFormatMismatchDirective(t *testing.T) {
	got := formatMismatchDirective(fmt.Errorf("share %q: %w", "/srv/share", block.ErrFutureFormat))
	// A mis-indexed verb renders as %!s(MISSING) or similar rather than
	// failing to compile, so the operator would get the artifact instead
	// of the error.
	if strings.Contains(got, "%!") {
		t.Fatalf("directive has a formatting artifact:\n%s", got)
	}
	for _, w := range []string{"newer release", "Upgrades are one-way", "/srv/share"} {
		if !strings.Contains(got, w) {
			t.Errorf("directive missing %q:\n%s", w, got)
		}
	}
}

// The guard returns the exit status rather than calling exitFn itself, so the
// caller's defers (notably the control-plane store close) run before the
// process exits. The returned code is what is asserted here.
func TestHandleFormatMismatch_MatchesEverySentinel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"block future format", block.ErrFutureFormat, true},
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
