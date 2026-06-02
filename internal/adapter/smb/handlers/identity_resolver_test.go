package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

type testStoreWithIDMap struct {
	store.Store
	mappings map[string]*models.IdentityMapping // keyed by "provider|principal"
}

func idmapKey(provider, principal string) string {
	return provider + "|" + principal
}

func (s *testStoreWithIDMap) GetIdentityMapping(_ context.Context, provider, principal string) (*models.IdentityMapping, error) {
	if mapping, ok := s.mappings[idmapKey(provider, principal)]; ok {
		return mapping, nil
	}
	return nil, models.ErrMappingNotFound
}

func (s *testStoreWithIDMap) ListIdentityMappings(_ context.Context, _ string) ([]*models.IdentityMapping, error) {
	return nil, nil
}

func (s *testStoreWithIDMap) CreateIdentityMapping(_ context.Context, mapping *models.IdentityMapping) error {
	if mapping.ProviderName == "" {
		mapping.ProviderName = "kerberos"
	}
	s.mappings[idmapKey(mapping.ProviderName, mapping.Principal)] = mapping
	return nil
}

func (s *testStoreWithIDMap) DeleteIdentityMapping(_ context.Context, provider, principal string) error {
	key := idmapKey(provider, principal)
	if _, ok := s.mappings[key]; !ok {
		return models.ErrMappingNotFound
	}
	delete(s.mappings, key)
	return nil
}

func (s *testStoreWithIDMap) ListIdentityMappingsForUser(_ context.Context, _ string) ([]*models.IdentityMapping, error) {
	return nil, nil
}

func TestFormatNTLMPrincipal(t *testing.T) {
	tests := []struct {
		domain   string
		username string
		want     string
	}{
		{"CORP", "alice", `CORP\alice`},
		{"", "alice", "alice"},
		{"EXAMPLE", "bob", `EXAMPLE\bob`},
	}

	for _, tt := range tests {
		got := formatNTLMPrincipal(tt.domain, tt.username)
		if got != tt.want {
			t.Errorf("formatNTLMPrincipal(%q, %q) = %q, want %q", tt.domain, tt.username, got, tt.want)
		}
	}
}

func TestResolveIdentityMapping(t *testing.T) {
	mockStore := &testStoreWithIDMap{
		mappings: map[string]*models.IdentityMapping{
			idmapKey("kerberos", `CORP\alice`):      {ProviderName: "kerberos", Principal: `CORP\alice`, Username: "alice-local"},
			idmapKey("kerberos", "bob@EXAMPLE.COM"): {ProviderName: "kerberos", Principal: "bob@EXAMPLE.COM", Username: "bob-local"},
			idmapKey("kerberos", "charlie"):         {ProviderName: "kerberos", Principal: "charlie", Username: "charlie-mapped"},
		},
	}

	rt := runtime.New(mockStore)
	h := &Handler{Registry: rt}

	tests := []struct {
		name      string
		principal string
		fallback  string
		wantUser  string
		wantFound bool
	}{
		{
			name:      "NTLM domain principal found",
			principal: `CORP\alice`,
			fallback:  "alice",
			wantUser:  "alice-local",
			wantFound: true,
		},
		{
			name:      "Kerberos principal found",
			principal: "bob@EXAMPLE.COM",
			fallback:  "bob",
			wantUser:  "bob-local",
			wantFound: true,
		},
		{
			name:      "Bare username fallback from DOMAIN\\user",
			principal: `OTHERDOMAIN\charlie`,
			fallback:  "charlie",
			wantUser:  "charlie-mapped",
			wantFound: true,
		},
		{
			name:      "No mapping found returns fallback",
			principal: "unknown@REALM",
			fallback:  "unknown",
			wantUser:  "unknown",
			wantFound: false,
		},
		{
			name:      "Empty fallback when no mapping (Kerberos path)",
			principal: "nobody@NOWHERE",
			fallback:  "",
			wantUser:  "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUser, gotFound := h.resolveIdentityMapping(context.Background(), tt.principal, tt.fallback)
			if gotUser != tt.wantUser || gotFound != tt.wantFound {
				t.Errorf("resolveIdentityMapping(%q, %q) = (%q, %v), want (%q, %v)",
					tt.principal, tt.fallback, gotUser, gotFound, tt.wantUser, tt.wantFound)
			}
		})
	}
}

func TestResolveIdentityMapping_NilStore(t *testing.T) {
	rt := runtime.New(nil)
	h := &Handler{Registry: rt}

	gotUser, gotFound := h.resolveIdentityMapping(context.Background(), "alice@EXAMPLE.COM", "alice")
	if gotUser != "alice" || gotFound != false {
		t.Errorf("expected (\"alice\", false), got (%q, %v)", gotUser, gotFound)
	}
}
