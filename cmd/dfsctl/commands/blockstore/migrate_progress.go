package blockstore

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/marmos91/dittofs/internal/logger"
)

// progressReporter emits a structured slog "migrate.file.committed"
// event on every per-file commit (D-A15) and, when stdout is
// attached to a TTY, overlays a `\r`-rewriting progress bar so
// operators get a live ETA on long runs.
//
// On non-TTY stdout (pipe, file redirect, CI logger) the bar is
// silenced — the slog event is the machine-friendly surface and
// stays the only output.
//
// Throttling: TTY refresh is rate-limited to one repaint per
// progressBarRefreshInterval to avoid flooding the terminal during
// fast, small-file workloads. The slog event fires unconditionally
// (one per commit).
type progressReporter struct {
	ttyEnabled bool
	total      int
	done       atomic.Int64
	startedAt  time.Time

	// out is the writer the TTY bar paints into. Defaults to
	// os.Stdout when unset; tests inject a *bytes.Buffer.
	out io.Writer

	// barMu serializes \r-paint calls so a parallel commit storm
	// can't interleave bar fragments. The slog call site is already
	// goroutine-safe via the shared logger.
	barMu sync.Mutex

	// lastPaintNs is the unix-nano timestamp of the last bar
	// repaint; reads / writes are atomic so the throttle decision
	// stays lock-free on the fast path.
	lastPaintNs atomic.Int64
}

// progressBarRefreshInterval throttles the TTY bar repaint to once
// per 100ms (10 fps). Below this the terminal can't keep up with
// repaints from a fast-commit workload anyway.
const progressBarRefreshInterval = 100 * time.Millisecond

// newProgressReporter detects whether stdout is a TTY and returns a
// reporter wired to os.Stdout. total is the file count the share
// walk produced; bar percentages are computed against it.
func newProgressReporter(total int) *progressReporter {
	tty := false
	if f, ok := any(os.Stdout).(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &progressReporter{
		ttyEnabled: tty,
		total:      total,
		startedAt:  time.Now(),
		out:        os.Stdout,
	}
}

// OnFileCommit is called once per successfully-committed file. It
// emits the slog event unconditionally and conditionally repaints
// the TTY bar (subject to throttling).
//
// Safe for concurrent calls from multiple worker goroutines.
func (p *progressReporter) OnFileCommit(r perFileResult) {
	if p == nil {
		return
	}
	done := p.done.Add(1)

	// Always emit the structured event — D-A15 baseline. Field set:
	// blocks_count, bytes_uploaded, bytes_deduped, files_done,
	// files_total. The handle is omitted from the event today
	// because the worker pool doesn't pass it through perFileResult;
	// callers wanting per-file traceability should attach the
	// handle to perFileResult in a follow-up plan.
	logger.Info("migrate.file.committed",
		"blocks_count", len(r.Blocks),
		"bytes_uploaded", r.BytesUploaded,
		"bytes_deduped", r.BytesDeduped,
		"files_done", done,
		"files_total", p.total,
	)

	if !p.ttyEnabled {
		return
	}
	// Throttle repaints to ~10 fps. The first commit always paints
	// (lastPaintNs == 0 fast-path).
	now := time.Now().UnixNano()
	last := p.lastPaintNs.Load()
	if last != 0 && time.Duration(now-last) < progressBarRefreshInterval {
		return
	}
	p.lastPaintNs.Store(now)

	p.paint(int(done))
}

// paint writes one \r-prefixed progress line to p.out under barMu.
// Format: `\rMigrating: D/T (PCT%) ETA E`.
func (p *progressReporter) paint(done int) {
	p.barMu.Lock()
	defer p.barMu.Unlock()
	if p.out == nil {
		return
	}
	pct := 0.0
	if p.total > 0 {
		pct = float64(done) / float64(p.total) * 100
	}
	eta := computeETA(p.startedAt, done, p.total)
	_, _ = fmt.Fprintf(p.out, "\rMigrating: %d/%d (%.1f%%) ETA %s", done, p.total, pct, eta)
}

// computeETA returns a coarse "remaining time" estimate based on the
// average per-file wall-clock so far. Returns "?" on done == 0 (no
// data) or total <= done (already complete / overshoot).
func computeETA(startedAt time.Time, done, total int) string {
	if done <= 0 || total <= done {
		return "?"
	}
	elapsed := time.Since(startedAt)
	perFile := elapsed / time.Duration(done)
	remaining := perFile * time.Duration(total-done)
	// Round to nearest second for human readability.
	return remaining.Round(time.Second).String()
}

// Close terminates the \r-overwrite line with a trailing newline so
// the next stdout writer (the printMigrateResult summary) starts on
// a fresh line. No-op when not in TTY mode.
func (p *progressReporter) Close() {
	if p == nil || !p.ttyEnabled {
		return
	}
	p.barMu.Lock()
	defer p.barMu.Unlock()
	if p.out != nil {
		_, _ = fmt.Fprintln(p.out)
	}
}
