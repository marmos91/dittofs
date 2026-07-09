package report

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/dfsbench/fio"
)

func TestRenderTable_HeadlineDashing(t *testing.T) {
	rs := []fio.CellResult{
		{System: "local", Workload: "seq-read", Size: "large", Protocol: "local", Pass: "warm", ThroughputMBps: 2400},
		{System: "local", Workload: "rand-read-4k", Size: "large", Protocol: "local", Pass: "warm", IOPS: 8940},
	}
	out := RenderTable(rs)
	if !strings.Contains(out, "2400") || !strings.Contains(out, "8940") {
		t.Errorf("missing values:\n%s", out)
	}
	// seq-read: IOPS column dashed; rand: MB/s column dashed.
	if !strings.Contains(out, "—") {
		t.Errorf("expected dashed non-headline cells:\n%s", out)
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
