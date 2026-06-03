package remote

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStack struct {
	outs map[string]string
	err  error
}

func (f fakeStack) Outputs(_ context.Context, _ string) (map[string]string, error) {
	return f.outs, f.err
}

func TestResolveTarget_FromOutputs(t *testing.T) {
	r := fakeStack{outs: map[string]string{
		OutputServerIP:        "203.0.113.7",
		OutputServerPrivateIP: "10.0.0.42",
		OutputPrivateNetID:    "pn-abc",
	}}
	tgt, err := ResolveTarget(context.Background(), r, "bench", "root", "", true)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if tgt.PublicIP != "203.0.113.7" || tgt.PrivateIP != "10.0.0.42" || tgt.User != "root" {
		t.Errorf("resolved target = %+v", tgt)
	}
}

func TestResolveTarget_PrivateOverrideWins(t *testing.T) {
	r := fakeStack{outs: map[string]string{
		OutputServerIP:        "1.1.1.1",
		OutputServerPrivateIP: "10.0.0.1",
	}}
	tgt, err := ResolveTarget(context.Background(), r, "bench", "root", "10.9.9.9", true)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if tgt.PrivateIP != "10.9.9.9" {
		t.Errorf("override ignored: %s", tgt.PrivateIP)
	}
}

func TestResolveTarget_NoPublicIP(t *testing.T) {
	r := fakeStack{outs: map[string]string{OutputServerPrivateIP: "10.0.0.1"}}
	if _, err := ResolveTarget(context.Background(), r, "bench", "root", "", true); err == nil {
		t.Fatal("missing serverIP must error")
	}
}

func TestResolveTarget_PrivateNetButNoPrivateIP(t *testing.T) {
	// NIC attached but private IP not surfaced → actionable error under
	// requirePrivate, telling the operator to pass --private-ip.
	r := fakeStack{outs: map[string]string{
		OutputServerIP:     "1.1.1.1",
		OutputPrivateNetID: "pn-xyz",
	}}
	_, err := ResolveTarget(context.Background(), r, "bench", "root", "", true)
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected private-IP error, got %v", err)
	}
}

func TestResolveTarget_DryRunToleratesMissingPrivate(t *testing.T) {
	// With requirePrivate=false (dry-run), a missing private IP is allowed so a
	// plan can still be printed.
	r := fakeStack{outs: map[string]string{OutputServerIP: "1.1.1.1"}}
	tgt, err := ResolveTarget(context.Background(), r, "bench", "root", "", false)
	if err != nil {
		t.Fatalf("dry-run resolve: %v", err)
	}
	if tgt.PrivateIP != "" {
		t.Errorf("expected empty private IP, got %s", tgt.PrivateIP)
	}
}

func TestResolveTarget_ReaderError(t *testing.T) {
	r := fakeStack{err: errors.New("pulumi down")}
	if _, err := ResolveTarget(context.Background(), r, "bench", "root", "", true); err == nil {
		t.Fatal("reader error must propagate")
	}
	if _, err := ResolveTarget(context.Background(), nil, "bench", "root", "", true); err == nil {
		t.Fatal("nil reader must error")
	}
}

func TestStringifyOutput(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hello", "hello"},
		{float64(42), "42"},
		{float64(3.5), "3.5"},
		{true, "true"},
		{[]any{"a", "b"}, `["a","b"]`},
	}
	for _, c := range cases {
		if got := stringifyOutput(c.in); got != c.want {
			t.Errorf("stringifyOutput(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
