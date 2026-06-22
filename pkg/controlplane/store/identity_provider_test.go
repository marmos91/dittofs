//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestIdentityProviderConfig_RoundTrip(t *testing.T) {
	s := createTestStore(t)
	ctx := context.Background()

	// Not-found before any write.
	if _, err := s.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP); !errors.Is(err, models.ErrIdentityProviderConfigNotFound) {
		t.Fatalf("expected ErrIdentityProviderConfigNotFound, got %v", err)
	}

	// Put with a secret-bearing config blob.
	const blob = `{"BindPassword":"s3cret","URL":"ldaps://dc"}`
	if err := s.PutIdentityProviderConfig(ctx, &models.IdentityProviderConfig{
		Type:    models.IdentityProviderTypeLDAP,
		Enabled: true,
		Config:  blob,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Enabled || got.Config != blob {
		t.Fatalf("round-trip mismatch: enabled=%v config=%q", got.Enabled, got.Config)
	}

	// Upsert replaces enabled + config on the same type (no duplicate row).
	const blob2 = `{"BindPassword":"new","URL":"ldaps://dc2"}`
	if err := s.PutIdentityProviderConfig(ctx, &models.IdentityProviderConfig{
		Type:    models.IdentityProviderTypeLDAP,
		Enabled: false,
		Config:  blob2,
	}); err != nil {
		t.Fatalf("Put upsert: %v", err)
	}
	got, err = s.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP)
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if got.Enabled || got.Config != blob2 {
		t.Fatalf("upsert mismatch: enabled=%v config=%q", got.Enabled, got.Config)
	}

	// Second provider type coexists.
	if err := s.PutIdentityProviderConfig(ctx, &models.IdentityProviderConfig{
		Type:    models.IdentityProviderTypeKerberos,
		Enabled: true,
		Config:  `{"KeytabPath":"/etc/x.keytab"}`,
	}); err != nil {
		t.Fatalf("Put kerberos: %v", err)
	}
	list, err := s.ListIdentityProviderConfigs(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}

	// Delete + not-found afterwards.
	if err := s.DeleteIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP); !errors.Is(err, models.ErrIdentityProviderConfigNotFound) {
		t.Fatalf("expected not-found after delete, got %v", err)
	}
	if err := s.DeleteIdentityProviderConfig(ctx, models.IdentityProviderTypeLDAP); !errors.Is(err, models.ErrIdentityProviderConfigNotFound) {
		t.Fatalf("expected not-found deleting missing row, got %v", err)
	}
}
