package orchestrator

import (
	"strings"
	"testing"
)

func TestDefaultManifestValid(t *testing.T) {
	if err := DefaultManifest().Validate(); err != nil {
		t.Fatalf("default manifest invalid: %v", err)
	}
}

func TestLoadManifest(t *testing.T) {
	in := `{"workloads":[
		{"name":"w1","workload":"sequential-write","ops":100,"seed":1},
		{"name":"w2","workload":"mixed-ops-storm","ops":200,"workers":4,"mix":"50,30,15,5","seed":2}
	]}`
	m, err := LoadManifest(strings.NewReader(in))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m.Workloads) != 2 {
		t.Fatalf("want 2 workloads, got %d", len(m.Workloads))
	}
	if m.Workloads[1].Workers != 4 || m.Workloads[1].Mix != "50,30,15,5" {
		t.Errorf("params not decoded: %+v", m.Workloads[1])
	}
}

func TestManifestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
	}{
		{"empty", Manifest{}},
		{"no-name", Manifest{Workloads: []WorkloadParams{{Workload: "x", Ops: 1}}}},
		{"no-workload", Manifest{Workloads: []WorkloadParams{{Name: "a", Ops: 1}}}},
		{"zero-ops", Manifest{Workloads: []WorkloadParams{{Name: "a", Workload: "x", Ops: 0}}}},
		{"dup-name", Manifest{Workloads: []WorkloadParams{
			{Name: "a", Workload: "x", Ops: 1}, {Name: "a", Workload: "y", Ops: 1}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.m.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}
