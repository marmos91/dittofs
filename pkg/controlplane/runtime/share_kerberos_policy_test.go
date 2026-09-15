package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// persistRequireKerberosShare writes a share row plus the NFS adapter config
// an operator would have left behind by setting require_kerberos while
// Kerberos was still configured.
func persistRequireKerberosShare(t *testing.T, ctx context.Context, s cpstore.Store, name string) {
	t.Helper()

	shareID, err := s.CreateShare(ctx, &models.Share{
		Name:            name,
		MetadataStoreID: "test-meta",
		BlockStoreID:    createBlockStoreConfig(t, s, "blocks"+name),
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	opts := models.DefaultNFSExportOptions()
	opts.RequireKerberos = true
	persistNFSExportOptions(t, ctx, s, shareID, opts)
}

// persistNoAuthSysShare writes the other way to reach an export no client can
// use: AUTH_SYS forbidden with no Kerberos to fall back to. It needs no
// decommissioning step to become unreachable — unlike require_kerberos, which
// is valid until the server loses Kerberos, this is already unusable the moment
// it is written on a server that has none.
func persistNoAuthSysShare(t *testing.T, ctx context.Context, s cpstore.Store, name string) {
	t.Helper()

	shareID, err := s.CreateShare(ctx, &models.Share{
		Name:            name,
		MetadataStoreID: "test-meta",
		BlockStoreID:    createBlockStoreConfig(t, s, "blocks"+name),
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	opts := models.DefaultNFSExportOptions()
	opts.AllowAuthSys = false
	opts.RequireKerberos = false
	persistNFSExportOptions(t, ctx, s, shareID, opts)
}

func persistNFSExportOptions(t *testing.T, ctx context.Context, s cpstore.Store, shareID string, opts models.NFSExportOptions) {
	t.Helper()
	cfg := &models.ShareAdapterConfig{ShareID: shareID, AdapterType: "nfs"}
	if err := cfg.SetConfig(opts); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.SetShareAdapterConfig(ctx, cfg); err != nil {
		t.Fatalf("SetShareAdapterConfig: %v", err)
	}
}

// serveNFS persists the enabled NFS adapter row a real server would have. The
// require_kerberos refusal gates an NFS-only export policy, so without this row
// every test below would assert against a server that never serves NFS and the
// refusal would be unreachable — green for the wrong reason.
func serveNFS(t *testing.T, ctx context.Context, s cpstore.Store) {
	t.Helper()
	if _, err := s.CreateAdapter(ctx, &models.AdapterConfig{
		Type:    "nfs",
		Enabled: true,
		Port:    models.DefaultNFSPort,
	}); err != nil {
		t.Fatalf("CreateAdapter(nfs): %v", err)
	}
}

// TestLoadSharesFromStore_RequireKerberosWithoutKerberosStops covers the
// sequence the config-time guard cannot see: require_kerberos is accepted
// while Kerberos is configured, Kerberos is then decommissioned, and the
// server restarts. The persisted policy is reachable by no auth flavor at
// all, so the share would export while refusing every client and SECINFO
// would narrow to an empty flavor list. The load must refuse instead.
func TestLoadSharesFromStore_RequireKerberosWithoutKerberosStops(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistRequireKerberosShare(t, ctx, s, "/krb-gone")

	// Kerberos has been decommissioned: the server comes up without it.
	rt.SetKerberosEnabled(false)

	err := LoadSharesFromStore(ctx, rt, s)
	if err == nil {
		t.Fatal("LoadSharesFromStore returned nil; want a refusal naming the share")
	}
	if !errors.Is(err, ErrKerberosNotConfigured) {
		t.Fatalf("error = %v; want errors.Is ErrKerberosNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "/krb-gone") {
		t.Fatalf("error %q does not name the share", err)
	}
	// The share is registered by the time the refusal is raised: whether the
	// policy is reachable depends on the share being served, which is only
	// known once AddShare has succeeded. Nothing reads that registration — the
	// sole caller aborts the boot, and adapters start after the share load, so
	// no listener exists to export it.
	if !rt.ShareExists("/krb-gone") {
		t.Fatal("fixture is wrong: the share was never added, so the refusal " +
			"cannot have come from the served-share check this test pins")
	}
}

// TestLoadSharesFromStore_RequireKerberosWithKerberosLoads pins the other
// half: the same persisted policy on a server that does have Kerberos is
// valid and must load. It sets the capability by hand, so it says nothing
// about the order the server establishes it in — that is pinned at the boot
// path itself, by TestLoadSharesWithKerberosCapability_PublishesBeforeLoading.
func TestLoadSharesFromStore_RequireKerberosWithKerberosLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistRequireKerberosShare(t, ctx, s, "/krb-ok")
	rt.SetKerberosEnabled(true)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}
	if !rt.ShareExists("/krb-ok") {
		t.Fatal("share with a satisfiable require_kerberos policy must load")
	}
}

// TestLoadSharesFromStore_RequireKerberosOnDisabledShareLoads keeps the
// refusal to shares that are actually served. MOUNT answers MNT3ERR_ACCES and
// PUTFH answers NFS4ERR_STALE on a disabled share whatever its auth policy
// says, so a disabled one carries no reachable misconfiguration and must not
// stop the whole server — an operator who disabled a share for maintenance
// would otherwise find the daemon refusing to boot over it.
func TestLoadSharesFromStore_RequireKerberosOnDisabledShareLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistRequireKerberosShare(t, ctx, s, "/krb-disabled")

	// Enabled is declared `default:true`, so the column has to be cleared with
	// an explicit update rather than at insert time.
	share, err := s.GetShare(ctx, "/krb-disabled")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	share.Enabled = false
	if err := s.UpdateShare(ctx, share); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if reread, err := s.GetShare(ctx, "/krb-disabled"); err != nil || reread.Enabled {
		t.Fatalf("share did not persist as disabled (err=%v)", err)
	}

	rt.SetKerberosEnabled(false)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore refused a disabled share: %v", err)
	}
}

// TestLoadSharesFromStore_RequireKerberosWithoutNFSLoads keeps the refusal off
// a server that does not serve NFS at all. require_kerberos is an NFS export
// policy: with no NFS adapter enabled no client can ever reach it, so an
// SMB-only deployment carrying the row from an earlier NFS life must still
// boot. Refusing it would strand that deployment on an upgrade, directing the
// operator at a policy that governs nothing it runs.
func TestLoadSharesFromStore_RequireKerberosWithoutNFSLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	// Deliberately no serveNFS: the adapter row is absent, as on a server whose
	// operator ran `dfsctl adapter disable nfs`.
	persistRequireKerberosShare(t, ctx, s, "/krb-smb-only")
	rt.SetKerberosEnabled(false)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore refused a share whose NFS policy no adapter serves: %v", err)
	}
	if !rt.ShareExists("/krb-smb-only") {
		t.Fatal("share must load: its require_kerberos policy is unreachable without NFS")
	}
}

// TestLoadSharesFromStore_RequireKerberosOnUnaddableShareLoads keeps the
// refusal off a share this load did not actually serve. A row naming a block
// store that is not configured is warned about and skipped, exporting nothing,
// so it carries no reachable misconfiguration and must not stop the server —
// the same exemption a disabled share gets, for the same reason.
func TestLoadSharesFromStore_RequireKerberosOnUnaddableShareLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistRequireKerberosShare(t, ctx, s, "/krb-unaddable")

	share, err := s.GetShare(ctx, "/krb-unaddable")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	share.BlockStoreID = "00000000-0000-0000-0000-000000000000"
	if err := s.UpdateShare(ctx, share); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}

	rt.SetKerberosEnabled(false)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore refused a share it could not add anyway: %v", err)
	}
	if rt.ShareExists("/krb-unaddable") {
		t.Fatal("fixture is wrong: the share was added, so this proves nothing about the skip path")
	}
}

// TestLoadSharesFromStore_NoAuthSysWithoutKerberosStops covers the second way to
// reach an export reachable by no auth flavor. allow_auth_sys=false produces the
// identical unreachable share as require_kerberos on a Kerberos-less server —
// AUTH_SYS and AUTH_NONE refused by the policy, RPCSEC_GSS impossible to
// negotiate — and used to be refused at neither write time nor load time.
func TestLoadSharesFromStore_NoAuthSysWithoutKerberosStops(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistNoAuthSysShare(t, ctx, s, "/no-authsys")
	rt.SetKerberosEnabled(false)

	err := LoadSharesFromStore(ctx, rt, s)
	if err == nil {
		t.Fatal("LoadSharesFromStore returned nil; a share that accepts no auth flavor must stop the boot")
	}
	if !errors.Is(err, ErrKerberosNotConfigured) {
		t.Fatalf("error = %v; want errors.Is ErrKerberosNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "/no-authsys") {
		t.Fatalf("error %q does not name the share", err)
	}
	// The remedy has to name the setting that actually caused it: telling an
	// operator to clear require_kerberos on a share that never set it sends
	// them to a flag that is already false.
	if !strings.Contains(err.Error(), "--allow-auth-sys true") {
		t.Errorf("error %q does not offer the remedy for the setting that caused it", err)
	}
}

// TestLoadSharesFromStore_NoAuthSysWithKerberosLoads pins the other half: with
// Kerberos available the same share is served by RPCSEC_GSS and is valid.
func TestLoadSharesFromStore_NoAuthSysWithKerberosLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	serveNFS(t, ctx, s)
	persistNoAuthSysShare(t, ctx, s, "/no-authsys-krb")
	rt.SetKerberosEnabled(true)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore refused a Kerberos-only share on a Kerberos server: %v", err)
	}
	if !rt.ShareExists("/no-authsys-krb") {
		t.Fatal("a Kerberos-only share must load when the server has Kerberos")
	}
}
