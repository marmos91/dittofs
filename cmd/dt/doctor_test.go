package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// An expired quarantine entry is an error from dt doctor, exit status 3,
// which names the entry, after its table of needs; other commands, such as
// dt list, still run.
func TestDoctorReportsAnExpiredQuarantine(t *testing.T) {
	checkout(t)
	writeFile(t, filepath.FromSlash(registry.QuarantinePath), "- { suite: s, test: t/flaky.sh, issue: '#1', expires: 2000-01-01 }\n")
	var status exitStatus
	out, err := dt(t, "doctor")
	if err == nil || errors.As(err, &status) || !strings.Contains(err.Error(), "t/flaky.sh expired on 2000-01-01") || !strings.HasPrefix(out, "NEED") {
		t.Errorf("dt doctor: %v, want its table and a registry error naming the expired entry:\n%s", err, out)
	}
	if _, err := dt(t, "list"); err != nil {
		t.Errorf("dt list: %v", err)
	}
}
