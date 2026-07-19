package backend

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

	plans, err := ResolveSystems([]string{"dittofs-s3"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	// Expands to all three managed protocols, in managedProtocols order.
	want := []string{"dittofs-s3-nfs3", "dittofs-s3-nfs4", "dittofs-s3-smb3"}
	if len(plans) != len(want) {
		t.Fatalf("got %d plans, want %d: %v", len(plans), len(want), plans)
	}
	for i, p := range plans {
		if p.SystemLabel() != want[i] {
			t.Errorf("plan[%d] = %q, want %q", i, p.SystemLabel(), want[i])
		}
		if p.Support != Native {
			t.Errorf("plan[%d] support = %s, want native", i, p.Support)
		}
	}
}

func TestResolveSystems_BareNameSkipsNAProtocols(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "kernel-nfs",                                                   // local-disk control, re-exported over knfsd only
		Support: map[Protocol]Support{ProtoNFS3: Reexport, ProtoNFS4: Reexport}, // no smb3
	})

	plans, err := ResolveSystems([]string{"kernel-nfs"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2 (smb3 is NA and must skip): %v", len(plans), plans)
	}
	for _, p := range plans {
		if p.Protocol == ProtoSMB3 {
			t.Errorf("smb3 must not appear for kernel-nfs: %v", p)
		}
		if p.Support != Reexport {
			t.Errorf("support = %s, want reexport", p.Support)
		}
	}
}

func TestResolveSystems_NativeSingleProtocol(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "zerofs", // native NFSv3 only (no nfs4/smb3)
		Support: map[Protocol]Support{ProtoNFS3: Native},
	})

	plans, err := ResolveSystems([]string{"zerofs"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	if len(plans) != 1 || plans[0].Protocol != ProtoNFS3 || plans[0].Support != Native {
		t.Fatalf("want single native nfs3 plan, got %v", plans)
	}
	// nfs4/smb3 named explicitly must be rejected, not silently produced.
	if _, err := ResolveSystems([]string{"zerofs-smb3"}); err == nil {
		t.Fatal("expected error for zerofs-smb3 (zerofs has no SMB)")
	}
}

func TestResolveSystems_ExplicitProtocol(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "dittofs-s3",
		Support: map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
	})

	plans, err := ResolveSystems([]string{"dittofs-s3-nfs4"})
	if err != nil {
		t.Fatalf("resolveSystems: %v", err)
	}
	if len(plans) != 1 || plans[0].Protocol != ProtoNFS4 {
		t.Fatalf("want single nfs4 plan, got %v", plans)
	}
}

func TestResolveSystems_ExplicitNARejected(t *testing.T) {
	withRegistry(t, &Backend{
		Name:    "kernel-nfs",
		Support: map[Protocol]Support{ProtoNFS3: Reexport},
	})

	if _, err := ResolveSystems([]string{"kernel-nfs-smb3"}); err == nil {
		t.Fatal("expected error for explicitly-named NA combo, got nil")
	}
}

func TestResolveSystems_UnknownBackend(t *testing.T) {
	withRegistry(t) // empty
	if _, err := ResolveSystems([]string{"nope"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if _, err := ResolveSystems([]string{"nope-nfs3"}); err == nil {
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

func TestManagedMatrix_WarmAllColdReadsOnly(t *testing.T) {
	plans := []Plan{
		{Backend: &Backend{Name: "dittofs-s3"}, Protocol: ProtoNFS3, Support: Native},
	}
	workloads := []string{"seq-write", "seq-read", "metadata"}
	cells := ManagedMatrix(plans, workloads, []string{"medium"}, true)

	// warm for all 3 + cold for the 1 read workload (seq-read) = 4 cells.
	if len(cells) != 4 {
		t.Fatalf("got %d cells, want 4: %v", len(cells), cells)
	}
	cold := 0
	for _, c := range cells {
		if c.Protocol != "nfs3" || c.System != "dittofs-s3-nfs3" {
			t.Errorf("bad stamp: %+v", c)
		}
		if c.Pass == "cold" {
			cold++
			if c.Workload != "seq-read" {
				t.Errorf("cold pass on non-read workload %q", c.Workload)
			}
		}
	}
	if cold != 1 {
		t.Errorf("got %d cold cells, want 1 (seq-read only)", cold)
	}
}

func TestManagedMatrix_NoEvictSkipsColdPass(t *testing.T) {
	plans := []Plan{{Backend: &Backend{Name: "b"}, Protocol: ProtoNFS3}}
	cells := ManagedMatrix(plans, []string{"seq-read"}, []string{"medium"}, false)
	if len(cells) != 1 || cells[0].Pass != "warm" {
		t.Fatalf("without evict expect single warm cell, got %v", cells)
	}
}

// TestDittofsDurabilityTiers asserts the three badger durability-tier variants
// (#1758) register under distinct names, all Native on every protocol, with
// distinct Tier strings — the axis the QoS matrix expands over.
func TestDittofsDurabilityTiers(t *testing.T) {
	tiers := map[string]bool{} // Tier string per variant, must be distinct
	for _, name := range []string{"dittofs-s3", "dittofs-s3-writeback", "dittofs-s3-remote"} {
		b, ok := registry[name]
		if !ok {
			t.Fatalf("backend %q not registered", name)
		}
		for _, p := range managedProtocols {
			if b.Support[p] != Native {
				t.Errorf("%s: protocol %s support = %s, want native", name, p, b.Support[p])
			}
		}
		if tiers[b.Tier] {
			t.Errorf("%s: duplicate Tier string %q", name, b.Tier)
		}
		tiers[b.Tier] = true
	}

	// Cache-cap writeback variants (cache-fill study) must also register with
	// native support. They share the writeback tier semantics, so they are not
	// part of the distinct-Tier check above.
	for _, name := range []string{"dittofs-s3-writeback-cap256m", "dittofs-s3-writeback-cap2g"} {
		b, ok := registry[name]
		if !ok {
			t.Errorf("backend %q not registered", name)
			continue
		}
		for _, p := range managedProtocols {
			if b.Support[p] != Native {
				t.Errorf("%s: protocol %s support = %s, want native", name, p, b.Support[p])
			}
		}
	}
}

// TestCompetitorVariants asserts the expanded competitor matrix (juicefs 6,
// rclone 2, s3fs 2, zerofs 2, goofys, ganesha) registers under the expected
// names with the right Support maps, and that each tool's two mode variants
// carry distinct Tier strings — so the run log records which durability tier was
// measured. Uses the real init-populated registry (like TestDittofsDurabilityTiers).
func TestCompetitorVariants(t *testing.T) {
	all := map[Protocol]Support{ProtoNFS3: Reexport, ProtoNFS4: Reexport, ProtoSMB3: Reexport}
	nfs3Native := map[Protocol]Support{ProtoNFS3: Native}
	nfs34Native := map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native}

	want := map[string]map[Protocol]Support{
		"juicefs":                  all,
		"juicefs-durable":          all,
		"juicefs-postgres":         all,
		"juicefs-postgres-durable": all,
		"juicefs-redis":            all,
		"juicefs-redis-durable":    all,
		"rclone":                   all,
		"rclone-cachefull":         all,
		"s3fs":                     all,
		"s3fs-nocache":             all,
		"goofys":                   all,
		"zerofs":                   nfs3Native,
		"zerofs-sync":              nfs3Native,
		"ganesha":                  nfs34Native,
		// cache-cap variants (3-scenario cache-fill study)
		"rclone-cap256m":  all,
		"rclone-cap2g":    all,
		"juicefs-cap256m": all,
		"juicefs-cap2g":   all,
		"s3fs-cap256m":    all,
		"s3fs-cap2g":      all,
	}
	for name, sup := range want {
		b, ok := registry[name]
		if !ok {
			t.Errorf("backend %q not registered", name)
			continue
		}
		for p, s := range sup {
			if b.Support[p] != s {
				t.Errorf("%s: protocol %s support = %s, want %s", name, p, b.Support[p], s)
			}
		}
		if len(sup) < 3 && b.Support[ProtoSMB3] != NA {
			t.Errorf("%s: smb3 support = %s, want na", name, b.Support[ProtoSMB3])
		}
		if b.Tier == "" {
			t.Errorf("%s: empty Tier string", name)
		}
	}

	// Each tool's two modes must carry distinct Tier strings.
	for _, p := range [][2]string{
		{"juicefs", "juicefs-durable"},
		{"rclone", "rclone-cachefull"},
		{"s3fs", "s3fs-nocache"},
		{"zerofs", "zerofs-sync"},
	} {
		if registry[p[0]].Tier == registry[p[1]].Tier {
			t.Errorf("%s and %s share Tier %q; modes must differ", p[0], p[1], registry[p[0]].Tier)
		}
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
