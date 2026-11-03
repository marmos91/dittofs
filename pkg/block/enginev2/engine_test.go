package enginev2

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate"
	"github.com/marmos91/dittofs/pkg/vfs/vfserr"
)

// fullDeps is a Deps with every member present. The interfaces are left
// unimplemented: New only checks that each is there.
func fullDeps() Deps {
	type (
		existence struct{ Existence }
		content   struct{ Content }
		lifecycle struct{ Lifecycle }
		opens     struct{ Opens }
		clock     struct{ Clock }
	)
	return Deps{
		Devices: []string{"/dev0"},
		Open: func(context.Context, string, journalv2.Version, journalv2.JournalID) (Journal, error) {
			return nil, nil
		},
		Existence: existence{},
		Content:   content{},
		Lifecycle: lifecycle{},
		Opens:     opens{},
		Closed:    func(entity.FileID, openstate.CloseResult) {},
		Clock:     clock{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, nil
		},
	}
}

func TestNewRefusesEachMissingMemberByName(t *testing.T) {
	if _, err := New(fullDeps()); err != nil {
		t.Fatalf("New(full Deps) = %v", err)
	}
	typ := reflect.TypeFor[Deps]()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		t.Run(name, func(t *testing.T) {
			d := fullDeps()
			reflect.ValueOf(&d).Elem().Field(i).SetZero()
			e, err := New(d)
			if err == nil || e != nil {
				t.Fatalf("New without %s = %v, %v; want refused", name, e, err)
			}
			if !strings.Contains(err.Error(), "Deps."+name+" ") {
				t.Fatalf("New without %s: error %q does not name it", name, err)
			}
		})
	}
}

func TestNewRefusesEmptyDevices(t *testing.T) {
	d := fullDeps()
	d.Devices = []string{}
	if _, err := New(d); err == nil || !strings.Contains(err.Error(), "Deps.Devices ") {
		t.Fatalf("New with no device = %v; want Devices refused", err)
	}
}

func TestApplyRefusesInvalidAndKeepsLast(t *testing.T) {
	e, err := New(fullDeps())
	if err != nil {
		t.Fatal(err)
	}
	good := Settings{ExistenceAge: time.Second, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}
	if err := e.Apply(good); err != nil {
		t.Fatalf("Apply(%+v) = %v", good, err)
	}
	for _, c := range []struct {
		key string
		bad func(*Settings)
	}{
		{"existence_age", func(s *Settings) { s.ExistenceAge = 0 }},
		{"existence_age", func(s *Settings) { s.ExistenceAge = -time.Second }},
		{"group_commit.max_files", func(s *Settings) { s.GroupCommitMaxFiles = 0 }},
		{"group_commit.max_files", func(s *Settings) { s.GroupCommitMaxFiles = -1 }},
		{"clone_max_len", func(s *Settings) { s.CloneMaxLen = 0 }},
		{"clone_max_len", func(s *Settings) { s.CloneMaxLen = -1 }},
	} {
		s := good
		s.ExistenceAge, s.GroupCommitMaxFiles, s.CloneMaxLen = 2*time.Second, 128, 1<<20
		c.bad(&s)
		err := e.Apply(s)
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("Apply(%+v) = %v; want refused naming %s", s, err, c.key)
		}
		if got := *e.settings.Load(); got != good {
			t.Errorf("after refused Apply(%+v): settings %+v, want the last %+v kept", s, got, good)
		}
	}
	next := Settings{ExistenceAge: 2 * time.Second, GroupCommitMaxFiles: 1, CloneMaxLen: 1}
	if err := e.Apply(next); err != nil {
		t.Fatalf("Apply(%+v) = %v", next, err)
	}
	if got := *e.settings.Load(); got != next {
		t.Fatalf("settings %+v after Apply, want %+v", got, next)
	}
}

// An unbuilt call answers one of the filesystem's errors, which its caller
// already maps, never one of the engine's own.
func TestUnbuiltIsUnavailable(t *testing.T) {
	e, err := New(fullDeps())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), nil); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("Start = %v; want vfserr.ErrUnavailable", err)
	}
}
