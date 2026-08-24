package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// captureCmdOut redirects command output into a buffer for the test's duration.
func captureCmdOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := exec.CmdOut
	exec.CmdOut = &buf
	t.Cleanup(func() { exec.CmdOut = prev })
	return &buf
}

// writeStubDfsctl puts a fake dfsctl at the front of PATH for the test. The
// stub is a POSIX shell script, and the harness only ever runs on the Linux
// bench VM, so the tests that drive the barrier through it are Linux-only.
func writeStubDfsctl(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub dfsctl is a POSIX shell script; the harness runs on the bench VM")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "dfsctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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
	buf := captureCmdOut(t)

	// A barrier that failed inside the drain reaches only the entry sample; the
	// dump must say which evidence is missing rather than print empty sections.
	d := dittofsBarrierDiag{entry: `{"totals":{"local_disk_used":1}}`}
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
	writeStubDfsctl(t, `#!/bin/sh
case "$*" in
  *"block stats"*) echo '{"totals":{"local_disk_used":5717934080,"unsynced_bytes":0,"pending_uploads":0,"eviction_suspended":true,"failed_syncs":7}}' ;;
  *"block evict"*) echo '{"local_files_evicted":0,"bytes_freed":0}' ;;
esac
exit 0
`)
	buf := captureCmdOut(t)

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
	// The label alone proves nothing: dump prints every label, and an unpopulated
	// step prints the same label followed by "not reached". This barrier reached
	// all four steps, so any "not reached" here means a capture was never wired
	// to the step it labels.
	if strings.Contains(out, "not reached") {
		t.Errorf("a step the barrier reached captured nothing:\n%s", out)
	}
}

func TestDittofsCaptureDistinguishesFailureFromUnattempted(t *testing.T) {
	// An empty string is the dump's marker for a step the barrier never reached,
	// so a step that WAS attempted must never render as one — the two lead to
	// opposite conclusions about where the barrier stopped.
	if got := dittofsCapture(nil, errors.New("exit 1")); !strings.Contains(got, "capture failed: exit 1") {
		t.Errorf("got %q, want a capture-failed note", got)
	}
	if got := dittofsCapture(nil, nil); got == "" {
		t.Error("a successful command with empty output must not render as unattempted")
	}
	if got := dittofsCapture([]byte("  {}\n"), nil); got != "{}" {
		t.Errorf("got %q, want the trimmed output", got)
	}
}

func TestDittofsEvictMarksAFailedCaptureAsFailed(t *testing.T) {
	writeStubDfsctl(t, `#!/bin/sh
case "$*" in
  *"block stats"*) echo '{"totals":{"local_disk_used":5717934080,"unsynced_bytes":0}}' ;;
  *"block evict"*) echo 'boom' >&2; exit 1 ;;
esac
exit 0
`)
	buf := captureCmdOut(t)

	if err := dittofsEvict(context.Background()); err == nil {
		t.Fatal("want an error when the evict command fails")
	}
	out := buf.String()
	if !strings.Contains(out, "evict result: (capture failed:") {
		t.Errorf("a failed evict must be reported as failed, not unattempted:\n%s", out)
	}
	// The steps after the failure genuinely were not reached, and must still say so.
	if !strings.Contains(out, "block stats after evict: not reached") {
		t.Errorf("dump lost the not-reached marker:\n%s", out)
	}
}

func TestDittofsResidentFilesReportsAPartialScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode 000 does not deny directory reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads the directory regardless of its mode")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.seg"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.seg"), make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unreadable subtree must not shrink the sample silently — the count would
	// then describe a tier that is emptier than the real one.
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	if got := dittofsResidentFiles(dir); !strings.Contains(got, "unreadable") {
		t.Errorf("got %q, want the partial scan flagged", got)
	}
}

// A drain residue is judged by the segments it can pin, not by its size: the
// pathological shape is a handful of MiB scattered one straggler per segment,
// which frees nothing while looking negligible in bytes.
func TestDittofsDrainResidueOK(t *testing.T) {
	const gib = int64(1) << 30
	tests := []struct {
		name          string
		unsynced      int64
		localResident int64
		want          bool
	}{
		{"fully synced", 0, 8 * gib, true},
		{"one straggler pins one segment out of a large tier", 1 << 20, 8 * gib, true},
		// <0.1% of a multi-GiB file by bytes, but spread thinly it pins ~32
		// segments (8 GiB) and the barrier's 80%-drop check then fails.
		{"small residue spread across many segments", 32 << 20, 8 * gib, false},
		{"same residue against a tier big enough to absorb the pin", 32 << 20, 128 * gib, true},
		{"whole tier pinnable", 4 << 20, gib, false},
		{"under the barrier floor the drop is never verified", 32 << 20, 32 << 20, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dittofsDrainResidueOK(tc.unsynced, tc.localResident); got != tc.want {
				t.Errorf("dittofsDrainResidueOK(%d, %d) = %v, want %v",
					tc.unsynced, tc.localResident, got, tc.want)
			}
		})
	}
}
