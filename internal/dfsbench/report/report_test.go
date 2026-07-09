package report

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/dfsbench/fio"
)

func TestRenderTable_HeadlineDashing(t *testing.T) {
	// AccessMode set so the ACCESS column isn't dashed — then the only "—" left in
	// each row is its non-headline rate column, so we assert rate-dashing per row
	// (not a table-wide Contains that any dash would satisfy).
	rs := []fio.CellResult{
		{System: "local-disk", AccessMode: "reexport", Workload: "seq-read", Size: "large", Protocol: "nfs3", Pass: "warm", ThroughputMBps: 2400},
		{System: "local-disk", AccessMode: "reexport", Workload: "rand-read-4k", Size: "large", Protocol: "nfs3", Pass: "warm", IOPS: 8940},
	}
	out := RenderTable(rs)
	var seqLine, randLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "seq-read") {
			seqLine = ln
		}
		if strings.Contains(ln, "rand-read-4k") {
			randLine = ln
		}
	}
	if !strings.Contains(seqLine, "2400") || !strings.Contains(seqLine, "—") {
		t.Errorf("seq-read row should show 2400 MB/s and a dashed IOPS:\n%s", seqLine)
	}
	if !strings.Contains(randLine, "8940") || !strings.Contains(randLine, "—") {
		t.Errorf("rand-read-4k row should show 8940 IOPS and a dashed MB/s:\n%s", randLine)
	}
}

func TestRenderPairing(t *testing.T) {
	rs := []fio.CellResult{
		{System: "dittofs-s3", AccessMode: "native", Workload: "seq-read", Size: "medium", Protocol: "nfs3", Pass: "cold", ThroughputMBps: 349, CtxSwPerSec: 18400},
		{System: "juicefs", AccessMode: "reexport", Workload: "seq-read", Size: "medium", Protocol: "nfs3", Pass: "cold", ThroughputMBps: 437, CtxSwPerSec: 96300},
		// A group with only a native row must NOT produce a pairing section.
		{System: "dittofs-s3", AccessMode: "native", Workload: "seq-write", Size: "medium", Protocol: "nfs3", Pass: "warm", ThroughputMBps: 500, CtxSwPerSec: 12000},
	}
	out := RenderPairing(rs)
	if !strings.Contains(out, "seq-read") || !strings.Contains(out, "18400") || !strings.Contains(out, "96300") {
		t.Errorf("pairing missing the paired seq-read cells:\n%s", out)
	}
	if strings.Contains(out, "seq-write") {
		t.Errorf("single-mode group must not be paired:\n%s", out)
	}
	// native row is ordered before the re-exported one.
	if strings.Index(out, "dittofs-s3") > strings.Index(out, "juicefs") {
		t.Errorf("native row must precede re-exported row:\n%s", out)
	}
}

func TestRenderPairing_NothingToPair(t *testing.T) {
	rs := []fio.CellResult{
		{System: "local", Workload: "seq-read", Protocol: "local", Pass: "warm"}, // no AccessMode
	}
	if out := RenderPairing(rs); out != "" {
		t.Errorf("want empty pairing when nothing pairs, got:\n%s", out)
	}
}

func TestRenderBaseline(t *testing.T) {
	// Zero value → nothing rendered (skipped/failed baseline).
	if out := RenderBaseline(Baseline{}); out != "" {
		t.Errorf("empty baseline should render nothing, got %q", out)
	}
	out := RenderBaseline(Baseline{LocalDiskSeqMBps: 2400, LocalDiskRandIOPS: 180000})
	if !strings.Contains(out, "2400") || !strings.Contains(out, "180000") {
		t.Errorf("baseline missing values:\n%s", out)
	}
	if !strings.Contains(out, "local-disk") {
		t.Errorf("baseline missing label:\n%s", out)
	}
}
