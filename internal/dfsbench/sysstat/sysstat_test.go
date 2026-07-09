package sysstat

import (
	"math"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// user=100 nice=0 system=50 idle=800 iowait=50 irq=0 softirq=0 steal=0
	// → total=1000, idle+iowait=850, busy=150.
	const data = "cpu  100 0 50 800 50 0 0 0 0 0\n" +
		"cpu0 100 0 50 800 50 0 0 0 0 0\n" +
		"intr 12345\n" +
		"ctxt 42000\n" +
		"btime 1600000000\n"
	s, ok := parseStat(data)
	if !ok {
		t.Fatal("parseStat: ok=false, want true")
	}
	if s.CtxSwitches != 42000 {
		t.Errorf("ctxt = %d, want 42000", s.CtxSwitches)
	}
	if s.CPUTotal != 1000 || s.CPUBusy != 150 {
		t.Errorf("cpu total/busy = %d/%d, want 1000/150", s.CPUTotal, s.CPUBusy)
	}
}

func TestParseStat_MissingFields(t *testing.T) {
	if _, ok := parseStat("ctxt 5\n"); ok { // no cpu line
		t.Error("want ok=false when cpu line absent")
	}
	if _, ok := parseStat("cpu 1 2 3 4 5\n"); ok { // no ctxt line
		t.Error("want ok=false when ctxt line absent")
	}
}

func TestParseStat_MalformedCPU(t *testing.T) {
	// A non-numeric cpu field must reject the whole sample, not return ok=true
	// with partial/bogus counters.
	if _, ok := parseStat("cpu 100 0 bad 800 50\nctxt 42000\n"); ok {
		t.Error("want ok=false when a cpu field is non-numeric")
	}
}

func TestRatesTo(t *testing.T) {
	t0 := time.Unix(0, 0)
	a := Sample{T: t0, CtxSwitches: 1000, CPUBusy: 100, CPUTotal: 1000, ok: true}
	// 2s later: +4000 ctxsw → 2000/s; busy +300 of total +400 → 75% CPU.
	b := Sample{T: t0.Add(2 * time.Second), CtxSwitches: 5000, CPUBusy: 400, CPUTotal: 1400, ok: true}
	r := a.RatesTo(b)
	if math.Abs(r.CtxSwPerSec-2000) > 1e-9 {
		t.Errorf("ctxsw/s = %v, want 2000", r.CtxSwPerSec)
	}
	if math.Abs(r.CPUPct-75) > 1e-9 {
		t.Errorf("cpu%% = %v, want 75", r.CPUPct)
	}
}

func TestRatesTo_Degenerate(t *testing.T) {
	ok := Sample{T: time.Unix(0, 0), CtxSwitches: 10, CPUBusy: 1, CPUTotal: 10, ok: true}
	// not-ok sample → zero rates (off-Linux path).
	if r := (Sample{}).RatesTo(ok); r != (Rates{}) {
		t.Errorf("not-ok source: got %+v, want zero", r)
	}
	if r := ok.RatesTo(Sample{}); r != (Rates{}) {
		t.Errorf("not-ok dest: got %+v, want zero", r)
	}
	// same timestamp → dt=0 → no divide-by-zero, zero ctxsw rate.
	same := ok
	same.CtxSwitches = 999
	if r := ok.RatesTo(same); r.CtxSwPerSec != 0 {
		t.Errorf("dt=0: ctxsw/s = %v, want 0", r.CtxSwPerSec)
	}
}
