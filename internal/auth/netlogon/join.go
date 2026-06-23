package netlogon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"unicode/utf16"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// userAccountControl flags (MS-ADTS 2.2.16). A computer (workstation trust)
// account is created with WORKSTATION_TRUST_ACCOUNT set. PASSWD_NOTREQD is NOT
// set: the account always carries a strong password we generate.
const (
	uacWorkstationTrustAccount = 0x1000

	// supportedEncTypes advertises the Kerberos enctypes the machine account
	// supports via msDS-SupportedEncryptionTypes (MS-KILE 2.2.7). 0x1F =
	// DES-CBC-CRC | DES-CBC-MD5 | RC4-HMAC | AES128-CTS | AES256-CTS. Setting it
	// is what `net ads join` does so the NETLOGON secure channel negotiates AES
	// (CapAES_SHA2); without it a Samba DC may negotiate a weaker channel and
	// reject the SamLogon passthrough. AES is also required for the password
	// rotation path (see password.go buildTrustPassword).
	supportedEncTypes = 0x1F
)

// JoinConfig configures the online creation of the machine (computer) account
// in Active Directory over LDAP. The directory write requires a privileged
// "join" credential (a Domain Admin or a delegated account with Create Computer
// Objects rights on the target OU).
//
// LDAPS (or StartTLS) is mandatory: AD refuses to write unicodePwd over an
// unencrypted connection, so the initial machine password can only be set on a
// confidential channel. Validate() enforces this.
type JoinConfig struct {
	// LDAPURL is the directory URL, "ldaps://dc.example.com" (preferred) or
	// "ldap://dc.example.com" combined with StartTLS.
	LDAPURL string
	// StartTLS upgrades a plaintext ldap:// connection before binding. Ignored
	// for ldaps://.
	StartTLS bool
	// BindDN / BindPassword are the privileged join credentials.
	BindDN       string
	BindPassword string
	// BaseDN is the domain naming context, e.g. "DC=dittofs,DC=ad". The computer
	// object is created under OU (if set) or directly under CN=Computers,<BaseDN>.
	BaseDN string
	// OU is the optional container DN for the computer object, e.g.
	// "OU=Servers,DC=dittofs,DC=ad". When empty the default CN=Computers
	// container is used.
	OU string
	// MachineName is the short computer name without the trailing '$'
	// (e.g. "DITTOFS"). The sAMAccountName becomes "<MachineName>$".
	MachineName string
	// DNSHostName is the FQDN stamped on the computer object (e.g.
	// "dittofs.dittofs.ad"). Optional but recommended; AD uses it for SPN
	// validation. When empty it is left unset.
	DNSHostName string
	// SPNs are the servicePrincipalName values to register on the account
	// (e.g. "cifs/dittofs.dittofs.ad"). Optional.
	SPNs []string
	// TLS holds client TLS settings for the LDAPS/StartTLS connection.
	TLS JoinTLSConfig
}

// JoinTLSConfig mirrors the LDAP provider's TLS knobs for the join connection.
type JoinTLSConfig struct {
	CACertFile         string
	InsecureSkipVerify bool
}

// Validate checks the join configuration before any network I/O.
func (c *JoinConfig) Validate() error {
	if strings.TrimSpace(c.LDAPURL) == "" {
		return fmt.Errorf("netlogon join: ldap_url is required")
	}
	lower := strings.ToLower(strings.TrimSpace(c.LDAPURL))
	switch {
	case strings.HasPrefix(lower, "ldaps://"):
		// Implicit TLS — confidential, AD will accept the unicodePwd write.
	case strings.HasPrefix(lower, "ldap://"):
		// Plaintext: AD rejects a unicodePwd write unless the channel is
		// encrypted, so StartTLS is mandatory here (there is no allow_plaintext
		// escape hatch for a join — a cleartext password write is never valid).
		if !c.StartTLS {
			return fmt.Errorf("netlogon join: ldap:// requires start_tls=true (AD refuses to set a machine password over an unencrypted connection)")
		}
	default:
		return fmt.Errorf("netlogon join: ldap_url must use ldap:// or ldaps:// (got %q)", c.LDAPURL)
	}
	if strings.TrimSpace(c.BindDN) == "" {
		return fmt.Errorf("netlogon join: bind_dn is required (a privileged account that can create computer objects)")
	}
	if c.BindPassword == "" {
		return fmt.Errorf("netlogon join: bind_password is required")
	}
	if strings.TrimSpace(c.BaseDN) == "" {
		return fmt.Errorf("netlogon join: base_dn is required")
	}
	if strings.TrimSpace(c.MachineName) == "" {
		return fmt.Errorf("netlogon join: machine_name is required")
	}
	if strings.ContainsAny(c.MachineName, "$ ,=\\/") {
		return fmt.Errorf("netlogon join: machine_name %q must be a bare NetBIOS label (no '$', spaces, or DN separators)", c.MachineName)
	}
	return nil
}

// ldapConn is the subset of *ldapv3.Conn the join uses. It is an interface so
// unit tests can supply a fake directory without a live DC.
type ldapConn interface {
	Bind(username, password string) error
	Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Add(req *ldapv3.AddRequest) error
	Modify(req *ldapv3.ModifyRequest) error
	Close() error
}

// ldapDialer opens a bound connection. Swapped in tests.
type ldapDialer func(ctx context.Context, cfg *JoinConfig) (ldapConn, error)

// computerDN returns the distinguished name of the computer object.
func (c *JoinConfig) computerDN() string {
	container := c.OU
	if strings.TrimSpace(container) == "" {
		container = "CN=Computers," + c.BaseDN
	}
	return "CN=" + c.MachineName + "," + container
}

// samAccountName returns the "<MachineName>$" machine-account name.
func (c *JoinConfig) samAccountName() string {
	return strings.ToUpper(c.MachineName) + "$"
}

// isLDAPS reports whether the configured URL uses implicit TLS (ldaps://).
func (c *JoinConfig) isLDAPS() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.LDAPURL)), "ldaps://")
}

// joinDirectory creates (or reconciles) the computer object and sets its
// password to newPassword, returning nothing on success. It is idempotent: if
// the object already exists, only the password (and any drifted SPNs) are reset,
// which is exactly what a re-join after a lost secret needs.
//
// The flow mirrors `net ads join`:
//  1. Bind as the privileged join account.
//  2. Search for an existing computer object by sAMAccountName.
//  3. Create it (objectClass=computer, sAMAccountName, userAccountControl,
//     dNSHostName, servicePrincipalName) if absent.
//  4. Set unicodePwd (AD requires the password as a UTF-16LE string wrapped in
//     double quotes, sent over the confidential connection).
func joinDirectory(ctx context.Context, dial ldapDialer, cfg *JoinConfig, newPassword string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	conn, err := dial(ctx, cfg)
	if err != nil {
		return fmt.Errorf("netlogon join: connect/bind: %w", err)
	}
	defer func() { _ = conn.Close() }()

	dn := cfg.computerDN()
	sam := cfg.samAccountName()

	exists, err := computerExists(conn, cfg, sam)
	if err != nil {
		return err
	}

	if !exists {
		if err := addComputerObject(conn, cfg, dn, sam); err != nil {
			return fmt.Errorf("netlogon join: create computer %q: %w", dn, err)
		}
	}

	if err := setUnicodePwd(conn, dn, newPassword); err != nil {
		return fmt.Errorf("netlogon join: set machine password on %q: %w", dn, err)
	}

	// Ensure the account is enabled with the workstation-trust UAC and advertises
	// AES enctypes. A freshly created object may be disabled until the password is
	// set, and an existing object from a prior (pre-AES) join may lack the enctype
	// attribute; reasserting both here makes the join converge regardless of the
	// create path, so the NETLOGON channel negotiates AES.
	mod := ldapv3.NewModifyRequest(dn, nil)
	mod.Replace("userAccountControl", []string{fmt.Sprintf("%d", uacWorkstationTrustAccount)})
	mod.Replace("msDS-SupportedEncryptionTypes", []string{fmt.Sprintf("%d", supportedEncTypes)})
	if err := conn.Modify(mod); err != nil {
		return fmt.Errorf("netlogon join: enable computer %q (uac + enctypes): %w", dn, err)
	}
	return nil
}

// computerExists reports whether a computer object with the given
// sAMAccountName already lives under BaseDN.
func computerExists(conn ldapConn, cfg *JoinConfig, sam string) (bool, error) {
	req := ldapv3.NewSearchRequest(
		cfg.BaseDN,
		ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases, 2, 0, false,
		fmt.Sprintf("(&(objectClass=computer)(sAMAccountName=%s))", ldapv3.EscapeFilter(sam)),
		[]string{"distinguishedName"}, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return false, fmt.Errorf("netlogon join: search computer %q: %w", sam, err)
	}
	return len(res.Entries) > 0, nil
}

// addComputerObject creates the computer object with the workstation-trust UAC
// and optional dNSHostName / SPNs.
func addComputerObject(conn ldapConn, cfg *JoinConfig, dn, sam string) error {
	add := ldapv3.NewAddRequest(dn, nil)
	add.Attribute("objectClass", []string{"top", "person", "organizationalPerson", "user", "computer"})
	add.Attribute("sAMAccountName", []string{sam})
	add.Attribute("userAccountControl", []string{fmt.Sprintf("%d", uacWorkstationTrustAccount)})
	add.Attribute("msDS-SupportedEncryptionTypes", []string{fmt.Sprintf("%d", supportedEncTypes)})
	if cfg.DNSHostName != "" {
		add.Attribute("dNSHostName", []string{cfg.DNSHostName})
	}
	if len(cfg.SPNs) > 0 {
		add.Attribute("servicePrincipalName", cfg.SPNs)
	}
	return conn.Add(add)
}

// setUnicodePwd sets the account password via the AD unicodePwd attribute. AD
// requires the value to be the password wrapped in double quotes and encoded as
// UTF-16LE, sent over a confidential (TLS) connection.
func setUnicodePwd(conn ldapConn, dn, password string) error {
	mod := ldapv3.NewModifyRequest(dn, nil)
	mod.Replace("unicodePwd", []string{encodeADPassword(password)})
	return conn.Modify(mod)
}

// encodeADPassword renders a password as the byte string AD expects for
// unicodePwd: the cleartext wrapped in double quotes, encoded UTF-16LE. The
// go-ldap library marshals the string's raw bytes as the attribute value, so
// the UTF-16LE bytes must be packed into a Go string here.
func encodeADPassword(password string) string {
	quoted := `"` + password + `"`
	codes := utf16.Encode([]rune(quoted))
	buf := make([]byte, 0, len(codes)*2)
	for _, c := range codes {
		buf = append(buf, byte(c), byte(c>>8))
	}
	return string(buf)
}

// dialAndBindJoin opens an encrypted LDAP connection and binds the join account.
// LDAPS connects with implicit TLS; ldap:// upgrades via StartTLS (required —
// Validate rejects plaintext joins).
func dialAndBindJoin(ctx context.Context, cfg *JoinConfig) (ldapConn, error) {
	host, err := joinHostFromURL(cfg.LDAPURL)
	if err != nil {
		return nil, err
	}
	isLDAPS := cfg.isLDAPS()

	if cfg.TLS.InsecureSkipVerify {
		// The machine password is written as unicodePwd over this connection. With
		// certificate verification disabled the channel is encrypted but the DC
		// identity is unverified, so a MITM could capture the cleartext password.
		// Acceptable only as a lab escape hatch — never in production.
		slog.Default().Warn("netlogon join: TLS certificate verification is DISABLED (insecure_skip_verify); the machine password is written over an unauthenticated TLS channel and is exposed to a man-in-the-middle. Use a pinned CA (ca_cert_file) in production.",
			"ldap_url", cfg.LDAPURL)
	}

	var opts []ldapv3.DialOpt
	if isLDAPS {
		tlsCfg, err := cfg.tlsClientConfig(host)
		if err != nil {
			return nil, err
		}
		opts = append(opts, ldapv3.DialWithTLSConfig(tlsCfg))
	}

	l, err := ldapv3.DialURL(cfg.LDAPURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.LDAPURL, err)
	}

	if !isLDAPS && cfg.StartTLS {
		tlsCfg, err := cfg.tlsClientConfig(host)
		if err != nil {
			_ = l.Close()
			return nil, err
		}
		if err := l.StartTLS(tlsCfg); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("ldap StartTLS upgrade failed: %w", err)
		}
	}

	if err := l.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("bind as %s: %w", cfg.BindDN, err)
	}
	return l, nil
}

// tlsClientConfig builds a *tls.Config for the join connection.
func (c *JoinConfig) tlsClientConfig(serverName string) (*tls.Config, error) {
	out := &tls.Config{
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.TLS.InsecureSkipVerify, //nolint:gosec // opt-in lab escape hatch, off by default
	}
	if c.TLS.CACertFile != "" {
		pem, err := os.ReadFile(c.TLS.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read join CA cert file %s: %w", c.TLS.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("join CA cert file %s contains no valid PEM certificate", c.TLS.CACertFile)
		}
		out.RootCAs = pool
	}
	return out, nil
}

// joinHostFromURL extracts the host (no port) for TLS ServerName verification.
func joinHostFromURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("netlogon join: parse url %q: %w", raw, err)
	}
	return u.Hostname(), nil
}
