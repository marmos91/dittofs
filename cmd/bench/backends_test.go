package main

import (
	"testing"
)

// withRegistry swaps in a fresh registry for the test and restores it after, so
// test backends don't leak into other tests (registry is a package global).
func withRegistry(t *testing.T, backends ...*Backend) {
	t.Helper()
	saved := registry
	registry = map[string]*Backend{}
	t.Cleanup(func() { registry = saved })
	for _, b := range backends {
		register(b)
	}
}

func TestResolveSystems_BareNameExpandsSupportedProtocols(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "dittofs-s3",
		Support: map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
	})

	plans, err := resolveSystems([]string{"dittofs-s3"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	// Expands to all three managed protocols, in managedProtocols order.
	want := []string{"dittofs-s3-nfs3", "dittofs-s3-nfs4", "dittofs-s3-smb3"}
	if len(plans) != len(want) {
		t.Fatalf("got %d plans, want %d: %v", len(plans), len(want), plans)
	}
	for i, p := range plans {
		if p.systemLabel() != want[i] {
			t.Errorf("plan[%d] = %q, want %q", i, p.systemLabel(), want[i])
		}
		if p.support != Native {
			t.Errorf("plan[%d] support = %s, want native", i, p.support)
		}
	}
}

func TestResolveSystems_BareNameSkipsNAProtocols(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "kernel-nfs",                                                   // local-disk control, re-exported over knfsd only
		Support: map[Protocol]Support{ProtoNFS3: Reexport, ProtoNFS4: Reexport}, // no smb3
	})

	plans, err := resolveSystems([]string{"kernel-nfs"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2 (smb3 is NA and must skip): %v", len(plans), plans)
	}
	for _, p := range plans {
		if p.protocol == ProtoSMB3 {
			t.Errorf("smb3 must not appear for kernel-nfs: %v", p)
		}
		if p.support != Reexport {
			t.Errorf("support = %s, want reexport", p.support)
		}
	}
}

func TestResolveSystems_ExplicitProtocol(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "dittofs-s3",
		Support: map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
	})

	plans, err := resolveSystems([]string{"dittofs-s3-nfs4"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	if len(plans) != 1 || plans[0].protocol != ProtoNFS4 {
		t.Fatalf("want single nfs4 plan, got %v", plans)
	}
}

func TestResolveSystems_ExplicitNARejected(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "kernel-nfs",
		Support: map[Protocol]Support{ProtoNFS3: Reexport},
	})

	if _, err := resolveSystems([]string{"kernel-nfs-smb3"}); err == nil {
		t.Fatal("expected error for explicitly-named NA combo, got nil")
	}
}

func TestResolveSystems_UnknownBackend(t *testing.T) {
	withRegistry(t) // empty
	if _, err := resolveSystems([]string{"nope"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if _, err := resolveSystems([]string{"nope-nfs3"}); err == nil {
		t.Fatal("expected error for unknown backend with protocol suffix")
	}
}

func TestSplitSystemLabel_HyphenatedBackendName(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "dittofs-s3", // name itself contains a hyphen
		Support: map[Protocol]Support{ProtoNFS3: Native},
	})

	// Bare hyphenated name: not explicit, backend resolves.
	b, _, explicit, err := splitSystemLabel("dittofs-s3")
	if err != nil || explicit || b.Name != "dittofs-s3" {
		t.Fatalf("bare: b=%v explicit=%v err=%v", b, explicit, err)
	}
	// With protocol suffix: peels only the protocol, keeps the hyphenated name.
	b, proto, explicit, err := splitSystemLabel("dittofs-s3-nfs3")
	if err != nil || !explicit || b.Name != "dittofs-s3" || proto != ProtoNFS3 {
		t.Fatalf("suffixed: b=%v proto=%v explicit=%v err=%v", b, proto, explicit, err)
	}
}

func TestBackendNamesSorted(t *testing.T) {
	withRegistry(t,
		&Backend{Name: "s3fs"},
		&Backend{Name: "juicefs"},
		&Backend{Name: "dittofs-s3"},
	)
	got := backendNames()
	want := []string{"dittofs-s3", "juicefs", "s3fs"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
