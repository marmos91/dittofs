// Package sysstat samples system-wide CPU and context-switch counters from
// /proc/stat, so each benchmark cell can report the serving stack's context
// switches per second — the measured evidence for the "DittoFS's native server
// avoids the FUSE context-switch tax" thesis.
//
// It is deliberately system-wide, not per-process: the one number that's
// comparable across every server type in the matrix — userspace (DittoFS,
// zerofs, the FUSE daemons) AND kernel-thread (knfsd) — is total ctxsw, which
// per-process /proc/<pid>/status can't give for kernel servers. On the
// disposable, single-tenant bench VM only the one backend + fio run, so the
// system-wide rate tracks the serving stack. It does include fio's own switches,
// but that load-generator cost is ~constant across cells at a fixed workload, so
// the cross-cell delta (native vs FUSE-over-knfsd) is still the FUSE tax.
package sysstat

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Sample is a point-in-time snapshot of /proc/stat's aggregate counters. The
// zero value has ok=false; off Linux (no /proc/stat) Now returns it, and RatesTo
// then yields zero rates — so the meter degrades to empty columns rather than
// erroring on a dev macOS --local/--smoke run.
type Sample struct {
	T           time.Time
	CtxSwitches uint64 // /proc/stat "ctxt": cumulative context switches since boot
	CPUBusy     uint64 // non-idle jiffies (user+nice+system+irq+softirq+steal)
	CPUTotal    uint64 // all jiffies, incl. idle+iowait
	ok          bool
}

// Now snapshots /proc/stat. On any read/parse failure it returns a not-ok
// Sample (rates from it are zero) — the caller never has to handle an error.
func Now() Sample {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Sample{}
	}
	s, ok := parseStat(string(data))
	if !ok {
		return Sample{}
	}
	s.T = time.Now()
	s.ok = true
	return s
}

// parseStat extracts the aggregate "cpu" and "ctxt" lines. Split out for
// testing without a live /proc.
func parseStat(data string) (Sample, bool) {
	var s Sample
	var haveCPU, haveCtxt bool
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "cpu": // aggregate line: "cpu user nice system idle iowait irq softirq steal ..."
			var total, idle uint64
			for i, f := range fields[1:] {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					break
				}
				total += v
				if i == 3 || i == 4 { // idle, iowait
					idle += v
				}
			}
			s.CPUTotal, s.CPUBusy, haveCPU = total, total-idle, true
		case "ctxt":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				s.CtxSwitches, haveCtxt = v, true
			}
		}
	}
	return s, haveCPU && haveCtxt
}

// Rates are per-cell derived metrics over the interval between two Samples.
type Rates struct {
	CtxSwPerSec float64 // Δctxt ÷ wall-seconds
	CPUPct      float64 // busy jiffies as % of total over the interval, 0..100
}

// RatesTo computes rates from a (earlier) to b (later). Returns zeros unless
// both samples are ok and the interval and CPU-jiffy denominator are positive —
// so a missing/degenerate sample can't produce NaN or a bogus spike.
func (a Sample) RatesTo(b Sample) Rates {
	if !a.ok || !b.ok {
		return Rates{}
	}
	dt := b.T.Sub(a.T).Seconds()
	var r Rates
	if dt > 0 && b.CtxSwitches >= a.CtxSwitches {
		r.CtxSwPerSec = float64(b.CtxSwitches-a.CtxSwitches) / dt
	}
	if dTotal := b.CPUTotal - a.CPUTotal; b.CPUTotal > a.CPUTotal && b.CPUBusy >= a.CPUBusy {
		r.CPUPct = float64(b.CPUBusy-a.CPUBusy) / float64(dTotal) * 100
	}
	return r
}
