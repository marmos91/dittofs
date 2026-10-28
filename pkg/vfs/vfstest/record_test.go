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

func TestRecordAnswersEachScriptedCallOnce(t *testing.T) {
	ft := &fakeT{}
	svc := vfstest.Record(ft)
	ctx := context.Background()
	svc.Expect(vfs.OpSeek).Return(int64(1), nil)
	svc.Expect(vfs.OpSeek).Return(int64(2), nil)
	for _, want := range []int64{1, 2} {
		if got, _ := svc.Seek(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, vfs.SeekData); got != want {
			t.Fatalf("Seek = %d, want %d", got, want)
		}
	}
	svc.AssertDone()
	if len(ft.errs) != 0 {
		t.Fatalf("reported %q, want nothing", ft.errs)
	}
}

// Null and an unscripted Record answer within each call's contract: one
// answer per reference tested, an end to what a client pages through, and
// no short write.
func TestUnscriptedAnswersKeepTheContract(t *testing.T) {
	ctx := context.Background()
	refs := make([]vfs.StateRef, 3)
	for name, svc := range map[string]vfs.Service{"Null": vfstest.Null{}, "Record": vfstest.Record(&fakeT{})} {
		if got := svc.TestState(ctx, vfs.Identity{}, 1, refs); len(got) != len(refs) {
			t.Errorf("%s: TestState answered %d, want %d", name, len(got), len(refs))
		}
		if p, _ := svc.ReadDir(ctx, vfs.Identity{}, nil, 0, 10, 0); !p.EOF {
			t.Errorf("%s: ReadDir answered no end", name)
		}
		if _, eof, _ := svc.Read(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, nil); !eof {
			t.Errorf("%s: Read answered no end", name)
		}
		if w, _ := svc.Write(ctx, vfs.Identity{}, vfs.Anonymous(nil), 0, make([]byte, 3), vfs.Unstable); w.N != 3 {
			t.Errorf("%s: Write of 3 bytes answered N %d", name, w.N)
		}
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
