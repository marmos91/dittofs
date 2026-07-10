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
	"regexp"
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
	// DiskWrBytes is the cumulative bytes written across whole-disk block devices
	// (/proc/diskstats sectors-written × 512). On the bench VM this is dominated by
	// the block volume holding the DittoFS local tier, so its rate is the tier's
	// fill throughput — the discriminator for "is a cold read disk-write bound?".
	DiskWrBytes uint64
	// NetRxBytes is cumulative bytes received across non-loopback interfaces
	// (/proc/net/dev). NFS/SMB to the fio client run over loopback here, so the
	// non-lo rate isolates the S3 DOWNLOAD rate — the "is a cold read S3-network
	// bound?" discriminator that pairs with DiskWrBytes.
	NetRxBytes uint64
	ok         bool
}

// wholeDiskRe matches whole-disk device names in /proc/diskstats (sda, vda,
// xvdb, nvme0n1) but not their partitions (sda1, nvme0n1p1) — summing both would
// double-count the same writes.
var wholeDiskRe = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+)$`)

// Now snapshots /proc/stat (required) plus /proc/diskstats and /proc/net/dev
// (best-effort). On a /proc/stat read/parse failure it returns a not-ok Sample
// (rates from it are zero) — the caller never has to handle an error. Missing
// disk/net files (macOS) leave those counters zero, so they degrade to empty
// columns exactly like the CPU/ctxsw pair.
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
	if d, err := os.ReadFile("/proc/diskstats"); err == nil {
		s.DiskWrBytes = parseDiskWriteBytes(string(d))
	}
	if n, err := os.ReadFile("/proc/net/dev"); err == nil {
		s.NetRxBytes = parseNetRxBytes(string(n))
	}
	return s
}

// parseDiskWriteBytes sums sectors-written (field 10, 512-byte sectors) across
// whole-disk devices in /proc/diskstats. Split out for testing without /proc.
func parseDiskWriteBytes(data string) uint64 {
	var total uint64
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !wholeDiskRe.MatchString(f[2]) {
			continue
		}
		if sectors, err := strconv.ParseUint(f[9], 10, 64); err == nil {
			total += sectors * 512
		}
	}
	return total
}

// parseNetRxBytes sums rx-bytes across non-loopback interfaces in /proc/net/dev.
// Split out for testing without /proc.
func parseNetRxBytes(data string) uint64 {
	var total uint64
	for _, line := range strings.Split(data, "\n") {
		iface, rest, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(iface) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 1 {
			continue
		}
		if rx, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			total += rx
		}
	}
	return total
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
			valid := true
			for i, f := range fields[1:] {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil { // corrupt /proc line — reject the whole sample, don't keep partial counters
					valid = false
					break
				}
				total += v
				if i == 3 || i == 4 { // idle, iowait
					idle += v
				}
			}
			if valid {
				s.CPUTotal, s.CPUBusy, haveCPU = total, total-idle, true
			}
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
	DiskWrMBps  float64 // whole-disk bytes written ÷ wall-seconds ÷ 1e6
	NetRxMBps   float64 // non-lo bytes received ÷ wall-seconds ÷ 1e6
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
	if dt > 0 && b.DiskWrBytes >= a.DiskWrBytes {
		r.DiskWrMBps = float64(b.DiskWrBytes-a.DiskWrBytes) / dt / 1e6
	}
	if dt > 0 && b.NetRxBytes >= a.NetRxBytes {
		r.NetRxMBps = float64(b.NetRxBytes-a.NetRxBytes) / dt / 1e6
	}
	return r
}
