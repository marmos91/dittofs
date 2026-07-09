package run

import (
	"strings"
	"testing"
)

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
		"--systems 'dittofs-s3,s3fs'", // forwarded values are shell-quoted
		"--workloads 'seq-read'",
		"--sizes 'large'",
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
