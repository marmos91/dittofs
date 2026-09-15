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

	shareID, err := s.CreateShare(ctx, &models.Share{Name: name, MetadataStoreID: "test-meta"})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	opts := models.DefaultNFSExportOptions()
	opts.RequireKerberos = true
	cfg := &models.ShareAdapterConfig{ShareID: shareID, AdapterType: "nfs"}
	if err := cfg.SetConfig(opts); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.SetShareAdapterConfig(ctx, cfg); err != nil {
		t.Fatalf("SetShareAdapterConfig: %v", err)
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
	if rt.ShareExists("/krb-gone") {
		t.Fatal("share must not be registered after a refused load")
	}
}

// TestLoadSharesFromStore_RequireKerberosWithKerberosLoads pins the other
// half: the same persisted policy on a server that does have Kerberos is
// valid and must load. It also pins the ordering the guard depends on —
// the server's Kerberos capability has to be known before shares load.
func TestLoadSharesFromStore_RequireKerberosWithKerberosLoads(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	persistRequireKerberosShare(t, ctx, s, "/krb-ok")
	rt.SetKerberosEnabled(true)

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}
	if !rt.ShareExists("/krb-ok") {
		t.Fatal("share with a satisfiable require_kerberos policy must load")
	}
}
