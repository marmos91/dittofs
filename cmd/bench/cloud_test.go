package main

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultVMSpec_EnvOverride(t *testing.T) {
	t.Setenv("SCW_INSTANCE_TYPE", "GP1-XS")
	t.Setenv("SCW_ZONE", "nl-ams-1")
	s := defaultVMSpec()
	if s.Type != "GP1-XS" || s.Zone != "nl-ams-1" {
		t.Fatalf("env override failed: %+v", s)
	}
	if s.Image != "ubuntu_noble" || s.RootVol != "sbs:100GB:5000" {
		t.Fatalf("defaults not applied: %+v", s)
	}
}

func TestVMState_RoundTrip(t *testing.T) {
	t.Chdir(t.TempDir()) // saveVM/loadVM use a cwd-relative file
	want := VM{ServerID: "srv-123", IP: "51.15.1.2", Zone: "fr-par-1"}
	if err := saveVM(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadVM()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip: got %+v want %+v", got, want)
	}
	if err := clearVMState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vmStateFile); !os.IsNotExist(err) {
		t.Fatal("state file should be gone after clearVMState")
	}
	if err := clearVMState(); err != nil {
		t.Fatalf("clearVMState must be idempotent, got %v", err)
	}
}

func TestRemoteRunArgs_ForwardsSelection(t *testing.T) {
	f := &runFlags{
		systems:    []string{"dittofs-s3", "s3fs"},
		workloads:  []string{"seq-read"},
		sizes:      []string{"large"},
		evictCache: true,
		resume:     true,
	}
	got := remoteRunArgs(f)
	for _, want := range []string{
		"/root/dfsbench run",
		"--config /root/dfsbench.yaml",
		"--results /root/bench-results",
		"--systems dittofs-s3,s3fs",
		"--workloads seq-read",
		"--sizes large",
		"--resume",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("remote args missing %q in: %s", want, got)
		}
	}
	// evictCache true is the default → no explicit flag.
	if strings.Contains(got, "--evict-cache") {
		t.Errorf("evict-cache default should not be forwarded: %s", got)
	}
}

func TestRemoteRunArgs_EvictDisabled(t *testing.T) {
	got := remoteRunArgs(&runFlags{systems: []string{"local-disk"}})
	if !strings.Contains(got, "--evict-cache=false") {
		t.Errorf("disabled evict must be forwarded explicitly: %s", got)
	}
}

func TestBuildDriver_DetachContract(t *testing.T) {
	d := buildDriver("/root/dfsbench run --systems local-disk")
	// The three properties that make a dropped ssh session survivable:
	for _, want := range []string{
		". /root/bench.env",    // creds sourced, not on argv
		"> /root/run.log 2>&1", // output captured for polling/tailing
		"touch /root/DONE",     // sentinel always dropped so polling terminates
	} {
		if !strings.Contains(d, want) {
			t.Errorf("driver missing %q:\n%s", want, d)
		}
	}
	// DONE must come after the run, else polling races the work.
	if strings.Index(d, "touch /root/DONE") < strings.Index(d, "run.log") {
		t.Error("DONE sentinel must be dropped after the run completes")
	}
}
