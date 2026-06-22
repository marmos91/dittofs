package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	krb5config "github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/identity/ldap"
)

// IdentityProviderHandler serves the admin-only identity-provider configuration
// API (LDAP/AD and Kerberos). Configuration is persisted in the control-plane
// database so it can be managed without editing config files or restarting the
// server; LDAP changes additionally hot-reload the live identity resolver.
type IdentityProviderHandler struct {
	store   store.IdentityProviderConfigStore
	runtime *runtime.Runtime
}

// NewIdentityProviderHandler constructs an IdentityProviderHandler.
func NewIdentityProviderHandler(s store.IdentityProviderConfigStore, rt *runtime.Runtime) *IdentityProviderHandler {
	return &IdentityProviderHandler{store: s, runtime: rt}
}

// --- Wire DTOs (snake_case; secrets are write-only) ---

type ldapTLSDTO struct {
	CACertFile         string `json:"ca_cert_file"`
	ClientCertFile     string `json:"client_cert_file"`
	ClientKeyFile      string `json:"client_key_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	MinVersion         string `json:"min_version"`
}

type ldapConfigDTO struct {
	Enabled         bool       `json:"enabled"`
	URL             string     `json:"url"`
	StartTLS        bool       `json:"start_tls"`
	AllowPlaintext  bool       `json:"allow_plaintext"`
	BaseDN          string     `json:"base_dn"`
	BindDN          string     `json:"bind_dn"`
	BindPassword    string     `json:"bind_password,omitempty"`
	UserAttr        string     `json:"user_attr"`
	Realm           string     `json:"realm"`
	Idmap           string     `json:"idmap"`
	NestedGroups    bool       `json:"nested_groups"`
	Timeout         string     `json:"timeout"`
	MaxGroupResults int        `json:"max_group_results"`
	TLS             ldapTLSDTO `json:"tls"`
}

// KerberosConfigDTO is the wire/persistence shape for Kerberos identity
// provider configuration. It is exported so cmd/dfs can decode the persisted
// row at startup and map it onto config.KerberosConfig — the handler cannot
// import pkg/config (which imports the control-plane API package, creating a
// cycle), so the config mapping lives in cmd. Durations are strings
// (Go time.Duration syntax, e.g. "5m", "8h").
type KerberosConfigDTO struct {
	Enabled          bool   `json:"enabled"`
	KeytabPath       string `json:"keytab_path"`
	ServicePrincipal string `json:"service_principal"`
	Realm            string `json:"realm"`
	NetBIOSDomain    string `json:"netbios_domain"`
	DNSDomain        string `json:"dns_domain"`
	Krb5Conf         string `json:"krb5_conf"`
	MaxClockSkew     string `json:"max_clock_skew"`
	ContextTTL       string `json:"context_ttl"`
	MaxContexts      int    `json:"max_contexts"`
}

type identityProviderSummary struct {
	Type       string `json:"type"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
}

type testResult struct {
	OK        bool   `json:"ok"`
	Stage     string `json:"stage,omitempty"`
	Message   string `json:"message,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// List reports the configured/enabled state of each identity provider type
// without exposing any secrets.
func (h *IdentityProviderHandler) List(w http.ResponseWriter, r *http.Request) {
	out := make([]identityProviderSummary, 0, 2)
	for _, t := range []string{models.IdentityProviderTypeLDAP, models.IdentityProviderTypeKerberos} {
		s := identityProviderSummary{Type: t}
		row, err := h.store.GetIdentityProviderConfig(r.Context(), t)
		switch {
		case err == nil:
			s.Configured = true
			s.Enabled = row.Enabled
		case errors.Is(err, models.ErrIdentityProviderConfigNotFound):
			// leave Configured=false
		default:
			InternalServerError(w, "Failed to list identity providers")
			return
		}
		out = append(out, s)
	}
	WriteJSONOK(w, out)
}

// GetConfig returns the stored configuration for a provider type with all
// secret material redacted. Returns 404 when the provider is not configured.
func (h *IdentityProviderHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	providerType := chi.URLParam(r, "type")
	row := h.loadRow(w, r, providerType)
	if row == nil {
		return // loadRow wrote the error/response
	}

	switch providerType {
	case models.IdentityProviderTypeLDAP:
		cfg, derr := ldap.UnmarshalStored([]byte(row.Config))
		if derr != nil {
			InternalServerError(w, "Failed to decode stored LDAP config")
			return
		}
		WriteJSONOK(w, ldapConfigToDTO(cfg))
	case models.IdentityProviderTypeKerberos:
		var dto KerberosConfigDTO
		if jerr := json.Unmarshal([]byte(row.Config), &dto); jerr != nil {
			InternalServerError(w, "Failed to decode stored Kerberos config")
			return
		}
		WriteJSONOK(w, dto) // Kerberos DTO carries no inline secret field
	}
}

// PutConfig creates or replaces the configuration for a provider type. The body
// is validated, persisted, and (for LDAP) hot-reloaded into the live resolver.
func (h *IdentityProviderHandler) PutConfig(w http.ResponseWriter, r *http.Request) {
	providerType := chi.URLParam(r, "type")
	switch providerType {
	case models.IdentityProviderTypeLDAP:
		h.putLDAP(w, r)
	case models.IdentityProviderTypeKerberos:
		h.putKerberos(w, r)
	default:
		NotFound(w, "Unknown identity provider type")
	}
}

// Test performs a non-persisting reachability/validity check for a provider.
func (h *IdentityProviderHandler) Test(w http.ResponseWriter, r *http.Request) {
	providerType := chi.URLParam(r, "type")
	switch providerType {
	case models.IdentityProviderTypeLDAP:
		h.testLDAP(w, r)
	case models.IdentityProviderTypeKerberos:
		h.testKerberos(w, r)
	default:
		NotFound(w, "Unknown identity provider type")
	}
}

// --- LDAP ---

func (h *IdentityProviderHandler) putLDAP(w http.ResponseWriter, r *http.Request) {
	var dto ldapConfigDTO
	if !decodeJSONBody(w, r, &dto) {
		return
	}
	cfg, err := h.ldapConfigFromDTO(r, &dto)
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	cfg.ApplyDefaults()
	if verr := cfg.Validate(); verr != nil {
		BadRequest(w, verr.Error())
		return
	}

	blob, err := ldap.MarshalStored(cfg)
	if err != nil {
		InternalServerError(w, "Failed to encode LDAP config")
		return
	}
	if err := h.store.PutIdentityProviderConfig(r.Context(), &models.IdentityProviderConfig{
		Type:    models.IdentityProviderTypeLDAP,
		Enabled: cfg.Enabled,
		Config:  string(blob),
	}); err != nil {
		InternalServerError(w, "Failed to persist LDAP config")
		return
	}

	// Hot-reload: update the runtime's live config and rebuild the resolver in
	// every adapter so the change takes effect without a restart.
	if h.runtime != nil {
		h.runtime.SetLDAPConfig(cfg)
		h.runtime.NotifyIdentityProviderConfigChange()
	}

	WriteJSONOK(w, ldapConfigToDTO(cfg))
}

func (h *IdentityProviderHandler) testLDAP(w http.ResponseWriter, r *http.Request) {
	var dto ldapConfigDTO
	if !decodeJSONBody(w, r, &dto) {
		return
	}
	cfg, err := h.ldapConfigFromDTO(r, &dto)
	if err != nil {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "validate", Message: err.Error()})
		return
	}
	// Validate up front so config errors are reported with Stage="validate";
	// only an actual dial/bind failure then surfaces as Stage="connect".
	cfg.ApplyDefaults()
	if verr := cfg.Validate(); verr != nil {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "validate", Message: verr.Error()})
		return
	}
	start := time.Now()
	if terr := ldap.TestConnection(r.Context(), cfg); terr != nil {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "connect", Message: terr.Error()})
		return
	}
	WriteJSON(w, http.StatusOK, testResult{OK: true, Message: "dial + bind succeeded", LatencyMS: time.Since(start).Milliseconds()})
}

// ldapConfigFromDTO maps a wire DTO to a domain ldap.Config, resolving the
// write-only bind password: an empty or redacted-placeholder password means
// "keep the currently stored password" so a read-modify-write round trip does
// not wipe the secret.
func (h *IdentityProviderHandler) ldapConfigFromDTO(r *http.Request, dto *ldapConfigDTO) (*ldap.Config, error) {
	cfg := &ldap.Config{
		Enabled:         dto.Enabled,
		URL:             dto.URL,
		StartTLS:        dto.StartTLS,
		AllowPlaintext:  dto.AllowPlaintext,
		BaseDN:          dto.BaseDN,
		BindDN:          dto.BindDN,
		BindPassword:    dto.BindPassword,
		UserAttr:        dto.UserAttr,
		Realm:           dto.Realm,
		Idmap:           ldap.IdmapMode(dto.Idmap),
		NestedGroups:    dto.NestedGroups,
		MaxGroupResults: dto.MaxGroupResults,
		TLS: ldap.TLSConfig{
			CACertFile:         dto.TLS.CACertFile,
			ClientCertFile:     dto.TLS.ClientCertFile,
			ClientKeyFile:      dto.TLS.ClientKeyFile,
			InsecureSkipVerify: dto.TLS.InsecureSkipVerify,
			MinVersion:         dto.TLS.MinVersion,
		},
	}
	if dto.Timeout != "" {
		d, err := time.ParseDuration(dto.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout %q: %w", dto.Timeout, err)
		}
		cfg.Timeout = d
	}
	if dto.BindPassword == "" || dto.BindPassword == redactedSecret {
		cfg.BindPassword = h.existingLDAPPassword(r)
	}
	return cfg, nil
}

// existingLDAPPassword returns the currently stored bind password, or "" if
// none/undecodable. Used to preserve the secret on read-modify-write.
func (h *IdentityProviderHandler) existingLDAPPassword(r *http.Request) string {
	row, err := h.store.GetIdentityProviderConfig(r.Context(), models.IdentityProviderTypeLDAP)
	if err != nil {
		return ""
	}
	cfg, err := ldap.UnmarshalStored([]byte(row.Config))
	if err != nil {
		return ""
	}
	return cfg.BindPassword
}

func ldapConfigToDTO(cfg *ldap.Config) ldapConfigDTO {
	dto := ldapConfigDTO{
		Enabled:         cfg.Enabled,
		URL:             cfg.URL,
		StartTLS:        cfg.StartTLS,
		AllowPlaintext:  cfg.AllowPlaintext,
		BaseDN:          cfg.BaseDN,
		BindDN:          cfg.BindDN,
		UserAttr:        cfg.UserAttr,
		Realm:           cfg.Realm,
		Idmap:           string(cfg.Idmap),
		NestedGroups:    cfg.NestedGroups,
		MaxGroupResults: cfg.MaxGroupResults,
		TLS: ldapTLSDTO{
			CACertFile:         cfg.TLS.CACertFile,
			ClientCertFile:     cfg.TLS.ClientCertFile,
			ClientKeyFile:      cfg.TLS.ClientKeyFile,
			InsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
			MinVersion:         cfg.TLS.MinVersion,
		},
	}
	if cfg.Timeout > 0 {
		dto.Timeout = cfg.Timeout.String()
	}
	// Write-only secret: report a placeholder when set, empty when unset.
	if cfg.BindPassword != "" {
		dto.BindPassword = redactedSecret
	}
	return dto
}

// --- Kerberos ---
//
// Kerberos config cannot reuse pkg/config here: pkg/config imports the
// control-plane API package, so importing it into a handler would create an
// import cycle. The handler therefore owns a plain DTO and validates inline;
// cmd/dfs maps the persisted DTO onto config.KerberosConfig at startup. A
// Kerberos config change applies on the next server restart (the NFS/SMB
// adapters consume it at startup), so PutConfig persists but does not
// hot-reload.

func (h *IdentityProviderHandler) putKerberos(w http.ResponseWriter, r *http.Request) {
	var dto KerberosConfigDTO
	if !decodeJSONBody(w, r, &dto) {
		return
	}
	if err := validateKerberosDTO(&dto); err != nil {
		BadRequest(w, err.Error())
		return
	}
	blob, err := json.Marshal(dto)
	if err != nil {
		InternalServerError(w, "Failed to encode Kerberos config")
		return
	}
	if err := h.store.PutIdentityProviderConfig(r.Context(), &models.IdentityProviderConfig{
		Type:    models.IdentityProviderTypeKerberos,
		Enabled: dto.Enabled,
		Config:  string(blob),
	}); err != nil {
		InternalServerError(w, "Failed to persist Kerberos config")
		return
	}
	// No hot-reload: Kerberos is bound by the adapters at startup. Surface that
	// the change takes effect on the next restart via a header note.
	w.Header().Set("X-DittoFS-Apply", "restart-required")
	WriteJSONOK(w, dto)
}

func (h *IdentityProviderHandler) testKerberos(w http.ResponseWriter, r *http.Request) {
	var dto KerberosConfigDTO
	if !decodeJSONBody(w, r, &dto) {
		return
	}
	if err := validateKerberosDTO(&dto); err != nil {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "validate", Message: err.Error()})
		return
	}
	if dto.KeytabPath == "" {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "validate", Message: "keytab_path is required"})
		return
	}
	kt, err := keytab.Load(dto.KeytabPath)
	if err != nil {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "keytab", Message: fmt.Sprintf("load keytab: %v", err)})
		return
	}
	if len(kt.Entries) == 0 {
		WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "keytab", Message: "keytab contains no entries"})
		return
	}
	if dto.Krb5Conf != "" {
		if _, cerr := krb5config.Load(dto.Krb5Conf); cerr != nil {
			WriteJSON(w, http.StatusOK, testResult{OK: false, Stage: "krb5conf", Message: fmt.Sprintf("load krb5.conf: %v", cerr)})
			return
		}
	}
	WriteJSON(w, http.StatusOK, testResult{OK: true, Message: fmt.Sprintf("keytab loaded (%d entries)", len(kt.Entries))})
}

// validateKerberosDTO mirrors the shape checks in config.KerberosConfig.Validate
// without importing pkg/config (which would create an import cycle).
func validateKerberosDTO(dto *KerberosConfigDTO) error {
	if dto.Enabled {
		if dto.KeytabPath == "" {
			return fmt.Errorf("keytab_path is required when kerberos is enabled")
		}
		if dto.ServicePrincipal == "" {
			return fmt.Errorf("service_principal is required when kerberos is enabled")
		}
	}
	if strings.ContainsAny(dto.Realm, "@/ ") {
		return fmt.Errorf("realm %q must not contain '@', '/', or spaces", dto.Realm)
	}
	if strings.ContainsAny(dto.NetBIOSDomain, ".@/ ") {
		return fmt.Errorf("netbios_domain %q must be a single label (no '.', '@', '/', or spaces)", dto.NetBIOSDomain)
	}
	if strings.ContainsAny(dto.DNSDomain, "@/ ") {
		return fmt.Errorf("dns_domain %q must not contain '@', '/', or spaces", dto.DNSDomain)
	}
	for name, v := range map[string]string{"max_clock_skew": dto.MaxClockSkew, "context_ttl": dto.ContextTTL} {
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", name, v, err)
		}
		// Match config.KerberosConfig.Validate, which rejects negatives.
		if d < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if dto.MaxContexts < 0 {
		return fmt.Errorf("max_contexts must be >= 0")
	}
	return nil
}

// --- shared ---

// loadRow fetches the config row for providerType, writing the appropriate
// error response and returning nil on miss/unknown-type/error.
func (h *IdentityProviderHandler) loadRow(w http.ResponseWriter, r *http.Request, providerType string) *models.IdentityProviderConfig {
	if providerType != models.IdentityProviderTypeLDAP && providerType != models.IdentityProviderTypeKerberos {
		NotFound(w, "Unknown identity provider type")
		return nil
	}
	row, err := h.store.GetIdentityProviderConfig(r.Context(), providerType)
	if err != nil {
		if errors.Is(err, models.ErrIdentityProviderConfigNotFound) {
			NotFound(w, "Identity provider not configured")
			return nil
		}
		InternalServerError(w, "Failed to read identity provider config")
		return nil
	}
	return row
}
