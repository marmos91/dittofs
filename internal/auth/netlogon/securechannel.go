package netlogon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	krb5_config "github.com/oiweiwei/gokrb5.fork/v9/config"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/smb2"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/gssapi"
	"github.com/oiweiwei/go-msrpc/ssp/krb5"
)

// gssapiRegister guards the one-time, process-global registration of the
// machine credential and SSP mechanisms in go-msrpc's gssapi stores.
// go-msrpc's gssapi.AddMechanism PANICS ("mechanism ... already exist") if a
// mechanism is registered twice, so this must run exactly once even across
// reconnects/resets.
var gssapiRegister sync.Once

// registerGSSAPI registers the machine credential and the SPNEGO/NTLM/Netlogon
// mechanisms with go-msrpc exactly once. The initial GetDCName/ReqChallenge
// bind inside NewSecureChannelClient authenticates the machine account via
// NTLM/SPNEGO (the netlogon schannel config does not exist yet at that point);
// the Netlogon mechanism is used only for the sealed secure channel afterward.
func registerGSSAPI(mc MachineCredential) {
	gssapiRegister.Do(func() {
		gssapi.AddCredential(credential.NewFromPassword(
			mc.AccountName, mc.Password, credential.Workstation(mc.Workstation)))
		gssapi.AddMechanism(ssp.SPNEGO)
		gssapi.AddMechanism(ssp.NTLM)
		// KRB5 authenticates the NETLOGON named-pipe SMB session: Samba rejects a
		// machine-account NTLM SMB logon, so the schannel must ride a Kerberos
		// SMB session over ncacn_np (#1345).
		gssapi.AddMechanism(ssp.KRB5)
		gssapi.AddMechanism(ssp.Netlogon)
	})
}

// buildKRB5Config returns an in-memory krb5 config that maps realm to the given
// KDC address, so the machine account can obtain its TGT and the cifs/ service
// ticket for the NETLOGON SMB session without a krb5.conf file on disk. The KDC
// is the DC we already connect to; DNS lookups are disabled since we point the
// realm straight at it.
func buildKRB5Config(realm, kdc string) (*krb5.Config, error) {
	realm = strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(realm), "."))
	if realm == "" {
		return nil, fmt.Errorf("netlogon: krb5 config: empty realm")
	}
	conf := fmt.Sprintf(`[libdefaults]
  default_realm = %[1]s
  dns_lookup_kdc = false
  dns_lookup_realm = false
  rdns = false
[realms]
  %[1]s = {
    kdc = %[2]s
    admin_server = %[2]s
  }
`, realm, kdc)
	parsed, err := krb5_config.NewFromString(conf)
	if err != nil {
		return nil, fmt.Errorf("netlogon: krb5 config: %w", err)
	}
	return &krb5.Config{KRB5Config: parsed}, nil
}

// SecureChannel wraps a go-msrpc schannel client with a mutex-guarded cached
// connection.  It is created lazily on the first NetworkLogon call.
type SecureChannel struct {
	mu     sync.Mutex
	cc     dcerpc.Conn
	cli    logon.LogonSecureChannelClient
	dcName string // UNC DC computer name (e.g. \\DC01); populated by connect() via GetDCName
}

// connect establishes the NETLOGON schannel connection to the given DC.
// Must be called with sc.mu held.
func (sc *SecureChannel) connect(ctx context.Context, mc MachineCredential) error {
	if sc.cli != nil {
		return nil
	}

	if len(mc.DCAddresses) == 0 {
		return fmt.Errorf("netlogon: no DC address configured")
	}
	server := mc.DCAddresses[0]

	// Discover the DC's canonical FQDN via the AD DNS SRV locator so we can name
	// the SMB Kerberos service principal (cifs/<fqdn>). The DC is the domain's
	// DNS server, so we query it directly at the address we already hold (#1324).
	// Connectivity still uses `server` (the IP) — only the SPN needs the name.
	dcs, err := DiscoverDCs(ctx, mc.Realm, server)
	if err != nil {
		return fmt.Errorf("netlogon: discover DC: %w", err)
	}
	spn := dcs[0].SPN()

	// Register the machine credential and the SPNEGO/NTLM/KRB5/Netlogon mechanisms
	// (once, process-global) BEFORE creating the security context, which captures
	// the registered credential and mechanisms. NewSecureChannelClient then runs
	// the full challenge handshake internally — so we must NOT pre-seed a
	// netlogon.Config.
	registerGSSAPI(mc)

	// Inline krb5 config (realm -> KDC = the DC) so the machine account can get a
	// TGT + cifs/ service ticket without depending on a host krb5.conf file.
	krb5Cfg, err := buildKRB5Config(mc.Realm, server)
	if err != nil {
		return err
	}
	gctx := gssapi.NewSecurityContext(ctx, ssp.WithKRB5(krb5Cfg))

	cc, err := dcerpc.Dial(gctx, server, epm.EndpointMapper(gctx, server))
	if err != nil {
		return fmt.Errorf("netlogon: dial %s: %w", server, err)
	}

	// Samba rejects the sealed-schannel AlterContext over ncacn_ip_tcp with
	// RPC_S_UNKNOWN_AUTHN_SERVICE (0x721); the named-pipe transport (\PIPE\netlogon
	// over SMB) is the binding Samba accepts. That SMB session must authenticate
	// the machine account with Kerberos (Samba refuses machine-account NTLM over
	// SMB), targeting the DC's cifs/ SPN (#1345).
	dialer := smb2.NewDialer(smb2.WithSecurity(gssapi.WithTargetName(spn)))
	cli, err := logon.NewSecureChannelClient(gctx, cc,
		dcerpc.WithSeal(),
		dcerpc.WithEndpoint("ncacn_np:"),
		dcerpc.WithSMBDialer(dialer),
	)
	if err != nil {
		_ = cc.Close(gctx)
		return fmt.Errorf("netlogon: secure channel client: %w", err)
	}

	sc.cc = cc
	sc.cli = cli

	// Discover the DC's computer name via GetDCName so samLogon can use the
	// correct UNC LogonServer (MS-NRPC requires the DC name, not the domain name).
	// Failure is non-fatal: we fall back to the domain-name form.
	dcResp, err := cli.GetDCName(gctx, &logon.GetDCNameRequest{
		ComputerName: mc.Workstation,
		DomainName:   mc.DomainName,
	})
	if err != nil || dcResp == nil || dcResp.DomainControllerInfo == nil || dcResp.DomainControllerInfo.DomainControllerName == "" {
		slog.Default().Debug("netlogon: GetDCName failed or returned empty name, falling back to domain name", "error", err)
		sc.dcName = "\\\\" + mc.DomainName
	} else {
		// DomainControllerName is already UNC-prefixed (e.g. \\DC01) per MS-NRPC.
		sc.dcName = dcResp.DomainControllerInfo.DomainControllerName
	}

	return nil
}

// close tears down the cached connection.  Must be called with sc.mu held.
func (sc *SecureChannel) close(ctx context.Context) {
	if sc.cc != nil {
		_ = sc.cc.Close(ctx)
		sc.cc = nil
	}
	sc.cli = nil
	sc.dcName = ""
}

// samLogon performs a NetrLogonSamLogon RPC call using the established channel.
// sc.mu is held for the full duration of the RPC so that concurrent NetworkLogon
// calls are serialized and close/reset cannot race an in-flight SAMLogon.
// Callers (ensureChannel/connect) must not hold sc.mu when calling samLogon.
func (sc *SecureChannel) samLogon(ctx context.Context, mc MachineCredential, req NetworkLogonRequest) (*LogonResult, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.cli == nil {
		return nil, fmt.Errorf("netlogon: channel not connected")
	}

	cli := sc.cli
	dcName := sc.dcName

	// Use the DC's computer name as LogonServer per MS-NRPC.
	// dcName is already UNC-prefixed (\\DC01) and set in connect(); fall back
	// to domain name if it was not discovered.
	logonServer := dcName
	if logonServer == "" {
		logonServer = "\\\\" + mc.DomainName
	}

	out, err := cli.SAMLogon(ctx, &logon.SAMLogonRequest{
		LogonServer:  logonServer,
		ComputerName: mc.Workstation,
		LogonLevel:   logon.LogonInfoClassNetworkTransitiveInformation,
		LogonInformation: &logon.Level{Value: &logon.Level_LogonNetworkTransitive{
			LogonNetworkTransitive: &logon.NetworkInfo{
				Identity: &logon.LogonIdentityInfo{
					ParameterControl: logon.IdentityAllowWorkstationTrustAccount,
					LogonDomainName:  &dtyp.UnicodeString{Buffer: req.Domain},
					UserName:         &dtyp.UnicodeString{Buffer: req.Username},
					Workstation:      &dtyp.UnicodeString{Buffer: mc.Workstation},
				},
				LMChallenge:         &logon.LMChallenge{Data: req.ServerChallenge[:]},
				NTChallengeResponse: &logon.String{Buffer: req.NTResponse},
				LMChallengeResponse: &logon.String{Buffer: req.LMResponse},
			},
		}},
		ValidationLevel: logon.ValidationInfoClassSAMInfo4,
	})
	if err != nil {
		return nil, fmt.Errorf("netlogon: SAMLogon: %w", err)
	}

	if out.Return != 0 {
		return nil, fmt.Errorf("netlogon: SAMLogon returned 0x%08x", uint32(out.Return))
	}

	// Extract ValidationSAMInfo4 from the union.
	v4, ok := out.ValidationInformation.Value.(*logon.Validation_SAM4)
	if !ok || v4 == nil || v4.ValidationSAM4 == nil {
		return nil, fmt.Errorf("netlogon: unexpected validation type %T", out.ValidationInformation.Value)
	}
	info := v4.ValidationSAM4

	var domainSID string
	if info.LogonDomainID != nil {
		domainSID = info.LogonDomainID.String()
	}

	groupRIDs := make([]uint32, 0, len(info.GroupIDs))
	for _, gm := range info.GroupIDs {
		if gm != nil {
			groupRIDs = append(groupRIDs, gm.RelativeID)
		}
	}

	// Session key: UserSessionKey.Data is []*CypherBlock, each block is 8 bytes.
	var sessionKey [16]byte
	if info.UserSessionKey != nil && len(info.UserSessionKey.Data) >= 2 {
		for i := 0; i < 2; i++ {
			blk := info.UserSessionKey.Data[i]
			if blk != nil {
				copy(sessionKey[i*8:], blk.Data)
			}
		}
	}

	return samInfo4ToResult(domainSID, info.UserID, groupRIDs, sessionKey, req.Username, req.Domain)
}
