package vfstest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
)

// fakeT keeps what the recorder reports instead of failing the test, and
// the cleanups it registers, which end runs.
type fakeT struct {
	testing.TB
	errs     []string
	cleanups []func()
}

func (f *fakeT) Helper()                        {}
func (f *fakeT) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }
func (f *fakeT) Cleanup(fn func())              { f.cleanups = append(f.cleanups, fn) }

// end runs the cleanups as testing does when a test ends, last first.
func (f *fakeT) end() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func TestRecordAnswersTheScriptedCall(t *testing.T) {
	ft := &fakeT{}
	svc := vfstest.Record(ft)
	ctx := context.Background()
	want := vfs.WriteResult{N: 4096}
	svc.Expect(vfs.OpWrite).Args(vfstest.Any, vfs.Identity{}, vfs.Anonymous(nil), int64(8192), []byte("x"), vfs.FileSync).
		Return(want, nil)

	svc.GetAttr(ctx, vfs.Identity{}, nil) // read-only and unscripted: answered, not refused
	got, err := svc.Write(ctx, vfs.Identity{}, vfs.Anonymous(nil), 8192, []byte("x"), vfs.FileSync)
	if got.N != want.N || err != nil {
		t.Fatalf("Write = %+v, %v; want %+v, nil", got, err, want)
	}
	svc.AssertDone()
	if len(ft.errs) != 0 {
		t.Fatalf("reported %q, want nothing", ft.errs)
	}
}

func TestRecordReportsWhatTheHandlerGotWrong(t *testing.T) {
	ctx := context.Background()
	for name, run := range map[string]func(svc *vfstest.Recorder){
		"other arguments": func(svc *vfstest.Recorder) {
			svc.Expect(vfs.OpCommit).Args(vfstest.Any, vfs.Identity{}, vfs.Anonymous(nil), int64(0), int64(0))
			svc.Commit(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, 1)
		},
		"unexpected mutation": func(svc *vfstest.Recorder) {
			svc.Destroy(ctx, vfs.Identity{}, 1)
		},
		"expected call never made": func(svc *vfstest.Recorder) {
			svc.Expect(vfs.OpCommit)
		},
		"two mutations": func(svc *vfstest.Recorder) {
			svc.Expect(vfs.OpCommit)
			svc.Expect(vfs.OpWrite)
			svc.Commit(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, 0)
			svc.Write(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, nil, vfs.Unstable)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ft := &fakeT{}
			svc := vfstest.Record(ft)
			run(svc)
			svc.AssertDone()
			if len(ft.errs) != 1 {
				t.Fatalf("reported %q, want one report", ft.errs)
			}
		})
	}
}

// A test that never calls AssertDone still fails on what it expected and
// was never called, once it ends; one that did is not reported again.
func TestRecordReportsUnmetExpectationsWhenTheTestEnds(t *testing.T) {
	ft := &fakeT{}
	svc := vfstest.Record(ft)
	svc.Expect(vfs.OpCommit)
	ft.end()
	if len(ft.errs) != 1 {
		t.Fatalf("reported %q at the end, want one report", ft.errs)
	}

	ft = &fakeT{}
	svc = vfstest.Record(ft)
	svc.Expect(vfs.OpCommit)
	svc.AssertDone()
	ft.end()
	if len(ft.errs) != 1 {
		t.Fatalf("reported %q, want AssertDone's report only", ft.errs)
	}
}
