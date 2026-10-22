package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// dt doctor fails on an expired quarantine entry with a registry error, exit
// status 3, which names the entry.
func TestDoctorRefusesAnExpiredQuarantine(t *testing.T) {
	checkout(t)
	writeFile(t, filepath.FromSlash(registry.QuarantinePath), "- { suite: s, test: t/flaky.sh, issue: '#1', expires: 2000-01-01 }\n")
	var status exitStatus
	if _, err := dt(t, "doctor"); err == nil || errors.As(err, &status) || !strings.Contains(err.Error(), "t/flaky.sh expired on 2000-01-01") {
		t.Errorf("dt doctor: %v, want a registry error naming the expired entry", err)
	}
}
