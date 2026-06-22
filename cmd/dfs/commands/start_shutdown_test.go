package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestIsExpectedShutdownErr pins #1329: a SIGTERM-initiated graceful shutdown
// returns context.Canceled (and listeners return http.ErrServerClosed), which
// must be classified as expected so it logs at Info and exits zero — while a
// genuine drain failure must still be treated as an error.
func TestIsExpectedShutdownErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"bare context.Canceled", context.Canceled, true},
		{"wrapped context.Canceled", fmt.Errorf("runtime serve: %w", context.Canceled), true},
		{"http.ErrServerClosed", http.ErrServerClosed, true},
		{"wrapped http.ErrServerClosed", fmt.Errorf("api server: %w", http.ErrServerClosed), true},
		{"real failure", errors.New("store flush failed"), false},
		{"deadline exceeded is not canceled", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpectedShutdownErr(tc.err); got != tc.want {
				t.Fatalf("isExpectedShutdownErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
