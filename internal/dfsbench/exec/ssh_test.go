package exec

import (
	"reflect"
	"testing"
)

// TestSSHBaseArgs pins the ssh/scp flag rendering: DisableAgent and KeyPath and
// ExtraOpts each map to their -o/-i form in order, and an empty config yields
// no flags. This is the one branch worth guarding in the salvaged executor.
func TestSSHBaseArgs(t *testing.T) {
	e := NewSSHExecutor(SSHConfig{
		KeyPath:      "/k.pem",
		DisableAgent: true,
		ExtraOpts:    []string{"StrictHostKeyChecking=accept-new"},
	}).(*sshExecutor)

	got := e.baseArgs()
	want := []string{"-o", "IdentityAgent=none", "-i", "/k.pem", "-o", "StrictHostKeyChecking=accept-new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs = %v, want %v", got, want)
	}

	if empty := NewSSHExecutor(SSHConfig{}).(*sshExecutor).baseArgs(); len(empty) != 0 {
		t.Fatalf("empty config should yield no args, got %v", empty)
	}
}
