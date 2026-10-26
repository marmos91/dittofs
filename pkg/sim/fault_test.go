package sim

import "testing"

func TestFaultKindString(t *testing.T) {
	cases := map[FaultKind]string{
		Pass:        "pass",
		Fail:        "fail",
		Delay:       "delay",
		Drop:        "drop",
		Tear:        "tear",
		Reorder:     "reorder",
		Reorder + 1: "FaultKind(6)",
		-1:          "FaultKind(-1)",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("FaultKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}
