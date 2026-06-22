package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// fakeIDPStore is an in-memory IdentityProviderConfigStore for handler tests.
type fakeIDPStore struct {
	rows     map[string]*models.IdentityProviderConfig
	putCalls int
}

func newFakeIDPStore() *fakeIDPStore {
	return &fakeIDPStore{rows: map[string]*models.IdentityProviderConfig{}}
}

func (s *fakeIDPStore) GetIdentityProviderConfig(_ context.Context, t string) (*models.IdentityProviderConfig, error) {
	if row, ok := s.rows[t]; ok {
		return row, nil
	}
	return nil, models.ErrIdentityProviderConfigNotFound
}

func (s *fakeIDPStore) ListIdentityProviderConfigs(context.Context) ([]*models.IdentityProviderConfig, error) {
	out := make([]*models.IdentityProviderConfig, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *fakeIDPStore) PutIdentityProviderConfig(_ context.Context, cfg *models.IdentityProviderConfig) error {
	s.putCalls++
	cp := *cfg
	s.rows[cfg.Type] = &cp
	return nil
}

func (s *fakeIDPStore) DeleteIdentityProviderConfig(_ context.Context, t string) error {
	if _, ok := s.rows[t]; !ok {
		return models.ErrIdentityProviderConfigNotFound
	}
	delete(s.rows, t)
	return nil
}

func withType(r *http.Request, t string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("type", t)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func doReq(t *testing.T, h func(http.ResponseWriter, *http.Request), method, body, providerType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/", strings.NewReader(body))
	r = withType(r, providerType)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

const validLDAPBody = `{
	"enabled": true,
	"url": "ldaps://dc.example.com:636",
	"base_dn": "DC=example,DC=com",
	"bind_dn": "CN=svc,DC=example,DC=com",
	"bind_password": "secret",
	"idmap": "rfc2307"
}`

func TestIDP_PutLDAP_PersistsAndRedacts(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil) // nil runtime: PutLDAP guards on it

	w := doReq(t, h.PutConfig, http.MethodPut, validLDAPBody, models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp ldapConfigDTO
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BindPassword != redactedSecret {
		t.Fatalf("response password = %q, want redacted", resp.BindPassword)
	}
	// Stored blob must contain the real password (usable after restart).
	row := st.rows[models.IdentityProviderTypeLDAP]
	if row == nil || !strings.Contains(row.Config, "secret") {
		t.Fatalf("stored config must retain real password, got %+v", row)
	}
}

func TestIDP_PutLDAP_InvalidConfigIs400(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)

	// enabled + plaintext ldap:// without StartTLS/AllowPlaintext → rejected.
	body := `{"enabled":true,"url":"ldap://dc","base_dn":"DC=x","bind_dn":"CN=s","bind_password":"p"}`
	w := doReq(t, h.PutConfig, http.MethodPut, body, models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if st.putCalls != 0 {
		t.Fatalf("invalid config must not persist (putCalls=%d)", st.putCalls)
	}
}

func TestIDP_GetLDAP_RedactsPassword(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)
	doReq(t, h.PutConfig, http.MethodPut, validLDAPBody, models.IdentityProviderTypeLDAP)

	w := doReq(t, h.GetConfig, http.MethodGet, "", models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("GET leaked password: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), redactedSecret) {
		t.Fatalf("GET should report redacted placeholder: %s", w.Body.String())
	}
}

func TestIDP_PutLDAP_PreservesPasswordOnRedactedResubmit(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)
	doReq(t, h.PutConfig, http.MethodPut, validLDAPBody, models.IdentityProviderTypeLDAP)

	// Re-submit with the redacted placeholder (as a GET-then-PUT round trip would).
	body := `{"enabled":true,"url":"ldaps://dc2","base_dn":"DC=example,DC=com","bind_dn":"CN=svc,DC=example,DC=com","bind_password":"********","idmap":"rfc2307"}`
	w := doReq(t, h.PutConfig, http.MethodPut, body, models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	row := st.rows[models.IdentityProviderTypeLDAP]
	if !strings.Contains(row.Config, "secret") {
		t.Fatalf("password must be preserved on redacted resubmit, got %s", row.Config)
	}
}

func TestIDP_GetLDAP_NotConfiguredIs404(t *testing.T) {
	h := NewIdentityProviderHandler(newFakeIDPStore(), nil)
	w := doReq(t, h.GetConfig, http.MethodGet, "", models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestIDP_UnknownTypeIs404(t *testing.T) {
	h := NewIdentityProviderHandler(newFakeIDPStore(), nil)
	w := doReq(t, h.GetConfig, http.MethodGet, "", "saml")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestIDP_List(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)
	doReq(t, h.PutConfig, http.MethodPut, validLDAPBody, models.IdentityProviderTypeLDAP)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.List(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got []identityProviderSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 providers, got %d", len(got))
	}
	byType := map[string]identityProviderSummary{}
	for _, s := range got {
		byType[s.Type] = s
	}
	if !byType["ldap"].Configured || !byType["ldap"].Enabled {
		t.Errorf("ldap should be configured+enabled: %+v", byType["ldap"])
	}
	if byType["kerberos"].Configured {
		t.Errorf("kerberos should be unconfigured: %+v", byType["kerberos"])
	}
}

func TestIDP_PutKerberos_ValidatesAndPersists(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)

	// Invalid realm shape → 400.
	bad := `{"enabled":false,"realm":"BAD/REALM"}`
	if w := doReq(t, h.PutConfig, http.MethodPut, bad, models.IdentityProviderTypeKerberos); w.Code != http.StatusBadRequest {
		t.Fatalf("bad realm status = %d, want 400", w.Code)
	}

	good := `{"enabled":false,"realm":"EXAMPLE.COM","netbios_domain":"EXAMPLE","dns_domain":"example.com","max_clock_skew":"5m"}`
	w := doReq(t, h.PutConfig, http.MethodPut, good, models.IdentityProviderTypeKerberos)
	if w.Code != http.StatusOK {
		t.Fatalf("good status = %d; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-DittoFS-Apply") != "restart-required" {
		t.Errorf("expected restart-required apply header")
	}
	if st.rows[models.IdentityProviderTypeKerberos] == nil {
		t.Fatalf("kerberos config not persisted")
	}
}

func TestIDP_TestLDAP_DoesNotPersist(t *testing.T) {
	st := newFakeIDPStore()
	h := NewIdentityProviderHandler(st, nil)

	// Points at an unreachable host: the test must return ok=false but never persist.
	body := `{"enabled":true,"url":"ldaps://127.0.0.1:1","base_dn":"DC=x","bind_dn":"CN=s","bind_password":"p","idmap":"rfc2307"}`
	w := doReq(t, h.Test, http.MethodPost, body, models.IdentityProviderTypeLDAP)
	if w.Code != http.StatusOK {
		t.Fatalf("test status = %d", w.Code)
	}
	var res testResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.OK {
		t.Errorf("expected ok=false for unreachable host")
	}
	if st.putCalls != 0 {
		t.Errorf("test must not persist (putCalls=%d)", st.putCalls)
	}
}
