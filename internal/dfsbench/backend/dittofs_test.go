package backend

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// writeLog writes n bytes of filler followed by tail and returns the path.
func writeLog(t *testing.T, filler int, tail string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, append(bytes.Repeat([]byte("x"), filler), tail...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDittofsLogSince(t *testing.T) {
	t.Run("returns only what was written after the offset", func(t *testing.T) {
		path := writeLog(t, 0, "before\nafter\n")
		got := dittofsLogSince(path, int64(len("before\n")))
		if got != "after\n" {
			t.Fatalf("got %q, want %q", got, "after\n")
		}
	})

	t.Run("says so when the server logged nothing", func(t *testing.T) {
		path := writeLog(t, 0, "before\n")
		got := dittofsLogSince(path, int64(len("before\n")))
		if !strings.Contains(got, "logged nothing") {
			t.Fatalf("got %q, want a nothing-logged note", got)
		}
	})

	t.Run("keeps the tail when the window exceeds the cap", func(t *testing.T) {
		// The window is everything after offset 0, which is far larger than the
		// cap; the bytes nearest the failure must survive, not the earliest ones.
		path := writeLog(t, dittofsBarrierLogTailBytes*2, "FAILURE WINDOW\n")
		got := dittofsLogSince(path, 0)
		if len(got) > dittofsBarrierLogTailBytes {
			t.Fatalf("window %d bytes exceeds the %d cap", len(got), dittofsBarrierLogTailBytes)
		}
		if !strings.HasSuffix(got, "FAILURE WINDOW\n") {
			t.Fatal("capped window dropped the most recent output")
		}
	})

	t.Run("never fails on a missing log", func(t *testing.T) {
		if got := dittofsLogSince(filepath.Join(t.TempDir(), "absent"), 0); !strings.Contains(got, "unreadable") {
			t.Fatalf("got %q, want an unreadable note", got)
		}
	})
}

func TestDittofsLogSizeMissingFileIsZero(t *testing.T) {
	if got := dittofsLogSize(filepath.Join(t.TempDir(), "absent")); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestDittofsBarrierDiagDumpMarksUnreachedSteps(t *testing.T) {
	var buf bytes.Buffer
	prev := exec.CmdOut
	exec.CmdOut = &buf
	defer func() { exec.CmdOut = prev }()

	// A barrier that failed inside the drain reaches only the entry sample; the
	// dump must say which evidence is missing rather than print empty sections.
	d := dittofsBarrierDiag{entry: []byte(`{"totals":{"local_disk_used":1}}`)}
	d.dump(errors.New("drain-uploads never fell below the floor"))

	out := buf.String()
	for _, want := range []string{
		"drain-uploads never fell below the floor",
		`{"totals":{"local_disk_used":1}}`,
		"block stats after drain (pre-evict): not reached",
		"evict result: not reached",
		"block stats after evict: not reached",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q:\n%s", want, out)
		}
	}
}
