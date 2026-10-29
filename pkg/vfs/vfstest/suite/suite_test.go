package suite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
)

func TestRunPassesNull(t *testing.T) {
	Run(t, func(t *testing.T) vfs.Service { return vfstest.Null{} })
}

func TestNeutral(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{vfs.ErrStale, true},
		{fmt.Errorf("lookup: %w", vfs.ErrUnavailable), true},
		{errors.New("vfs: handle names nothing"), false}, // the same text is not the sentinel
	} {
		if got := neutral(c.err); got != c.want {
			t.Errorf("neutral(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// raw answers errors that are none of vfs.Errors, one alone and one in a list.
type raw struct{ vfstest.Null }

func (raw) Unwatch(context.Context, vfs.Call, vfs.WatchID) error { return errors.New("raw") }

func (raw) TestState(context.Context, vfs.Call, vfs.ClientID, []vfs.StateRef) []error {
	return []error{errors.New("raw")}
}

// The neutral-errors case reports an error that is none of vfs.Errors, alone
// or in a list, and nothing else.
func TestNonNeutralReportsEachRawError(t *testing.T) {
	got := nonNeutral(t.Context(), raw{})
	slices.Sort(got)
	want := []string{
		"TestState answered raw, which is none of vfs.Errors",
		"Unwatch answered raw, which is none of vfs.Errors",
	}
	if !slices.Equal(got, want) {
		t.Errorf("nonNeutral reported %q, want %q", got, want)
	}
}
