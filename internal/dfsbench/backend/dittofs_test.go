package backend

import (
	"bytes"
	"context"
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

func TestDittofsResidentFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{
		"share/000001.seg": 3 << 20,
		"share/000002.seg": 1 << 20,
		"small":            0,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := dittofsResidentFiles(dir)
	// Largest first, so the shape of the surviving set is legible at a glance.
	if !strings.Contains(got, "3 files, 4MiB total") {
		t.Errorf("missing totals: %s", got)
	}
	if i, j := strings.Index(got, "000001.seg"), strings.Index(got, "000002.seg"); i < 0 || j < 0 || i > j {
		t.Errorf("files not ordered largest-first: %s", got)
	}

	if got := dittofsResidentFiles(filepath.Join(dir, "absent")); !strings.Contains(got, "unreadable") {
		t.Errorf("got %q, want an unreadable note", got)
	}
	if got := dittofsResidentFiles(t.TempDir()); !strings.Contains(got, "is empty") {
		t.Errorf("got %q, want an empty-dir note", got)
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

// TestDittofsEvictDumpsDiagnosticsOnFailure drives the whole barrier against a
// stub dfsctl that reports a local tier which never shrinks — the field failure
// this dump exists for. It pins the wiring: that a failed barrier prints the raw
// stats (including the fields the parsed totals drop), what the evict itself
// reported, and that a missing server log degrades to a note rather than a
// second failure.
func TestDittofsEvictDumpsDiagnosticsOnFailure(t *testing.T) {
	bin := t.TempDir()
	stub := filepath.Join(bin, "dfsctl")
	script := `#!/bin/sh
case "$*" in
  *"block stats"*) echo '{"totals":{"local_disk_used":5717934080,"unsynced_bytes":0,"pending_uploads":0,"eviction_suspended":true,"failed_syncs":7}}' ;;
  *"block evict"*) echo '{"local_files_evicted":0,"bytes_freed":0}' ;;
esac
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	prev := exec.CmdOut
	exec.CmdOut = &buf
	defer func() { exec.CmdOut = prev }()

	err := dittofsEvict(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cold barrier failed") {
		t.Fatalf("got %v, want a cold-barrier failure", err)
	}

	out := buf.String()
	for _, want := range []string{
		"cold barrier diagnostics",
		`"eviction_suspended":true`, // dropped by dittofsBlockTotals; kept by the raw capture
		`"failed_syncs":7`,
		"block stats after drain (pre-evict)",
		`"bytes_freed":0`,
		"server log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q:\n%s", want, out)
		}
	}
}
