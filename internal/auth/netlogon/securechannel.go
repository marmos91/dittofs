package netlogon

import (
	"context"
	"fmt"
	"strings"
	"sync"

	krb5_config "github.com/oiweiwei/gokrb5.fork/v9/config"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/smb2"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/gssapi"
	"github.com/oiweiwei/go-msrpc/ssp/krb5"
)

// gssapiMechRegister guards the one-time, process-global registration of the
// SSP mechanisms in go-msrpc's gssapi mechanism store. go-msrpc's
// gssapi.AddMechanism PANICS ("mechanism ... already exist") if a mechanism is
// registered twice, so this must run exactly once even across reconnects/resets.
//
// The machine CREDENTIAL is deliberately NOT registered here: it is passed
// per-connection via gssapi.WithCredential when building the security context in
// connect (see below). Registering it in a sync.Once would freeze the FIRST
// credential for the life of the process, so a machine-password rotation
// (#1325) would never reach go-msrpc and rebuilt channels would keep
// authenticating with the stale password.
var gssapiMechRegister sync.Once

// registerGSSAPIMechanisms registers the SPNEGO/NTLM/KRB5/Netlogon mechanisms
// with go-msrpc exactly once (process-global). The initial ReqChallenge bind
// inside NewSecureChannelClient authenticates the machine account via
// NTLM/SPNEGO (the netlogon schannel config does not exist yet at that point);
// the Netlogon mechanism is used only for the sealed secure channel afterward.
// KRB5 authenticates the NETLOGON named-pipe SMB session: Samba rejects a
// machine-account NTLM SMB logon, so the schannel must ride a Kerberos SMB
// session over ncacn_np (#1345). It registers mechanisms ONLY — the machine
// credential is supplied per-connection in connect so it can be hot-reloaded.
func registerGSSAPIMechanisms() {
	gssapiMechRegister.Do(func() {
		gssapi.AddMechanism(ssp.SPNEGO)
		gssapi.AddMechanism(ssp.NTLM)
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
	kdc = strings.TrimSpace(kdc)
	if kdc == "" {
		return nil, fmt.Errorf("netlogon: krb5 config: empty KDC address")
	}
	// realm and kdc are interpolated into the krb5 config text, so reject any
	// whitespace/control characters that could inject additional config
	// directives (e.g. a newline in a misconfigured dc_address).
	if strings.ContainsAny(realm, " \t\r\n[]{}") {
		return nil, fmt.Errorf("netlogon: krb5 config: invalid realm %q", realm)
	}
	if strings.ContainsAny(kdc, " \t\r\n[]{}") {
		return nil, fmt.Errorf("netlogon: krb5 config: invalid KDC address %q", kdc)
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
	dcName string // UNC DC name (e.g. \\DC01) for the LogonServer; derived locally by connect() (see deriveLogonServer)
}

// connect establishes the NETLOGON schannel connection to the given DC. It is
// self-locking (takes sc.mu) and idempotent: a no-op when already connected. The
// lock is held for the full handshake so a concurrent close cannot tear the
// connection down mid-build.
func (sc *SecureChannel) connect(ctx context.Context, mc MachineCredential) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.cli != nil {
		return nil
	}

	// Resolve the dial target and the Kerberos SMB service principal (cifs/<fqdn>)
	// for the DC we will actually connect to. The SPN must name the dialed host —
	// a cifs/<other-dc> ticket would not authenticate the SMB session (#1324).
	server, spn, err := resolveDialTarget(ctx, mc)
	if err != nil {
		return fmt.Errorf("netlogon: %w", err)
	}

	// Register the SPNEGO/NTLM/KRB5/Netlogon mechanisms (once, process-global)
	// BEFORE creating the security context. NewSecureChannelClient then runs the
	// full challenge handshake internally — so we must NOT pre-seed a
	// netlogon.Config.
	registerGSSAPIMechanisms()

	// Inline krb5 config (realm -> KDC = the DC) so the machine account can get a
	// TGT + cifs/ service ticket without depending on a host krb5.conf file.
	krb5Cfg, err := buildKRB5Config(mc.Realm, server)
	if err != nil {
		return err
	}

	gctx := gssapi.NewSecurityContext(ctx, ssp.WithKRB5(krb5Cfg))

	// Build the machine credential PER-CONNECTION and supply it via
	// dcerpc.WithCredentials (NOT a process-global gssapi.AddCredential under a
	// sync.Once). This is what makes machine-password rotation (#1325) take
	// effect: a channel rebuilt after a ReloadCredential authenticates with the
	// CURRENT mc, not a credential frozen at process start.
	//
	// The credential must reach BOTH security contexts of this connection:
	//   - the Kerberos SMB session (the schannel rides \PIPE\netlogon over SMB,
	//     and Samba refuses a machine-account NTLM SMB logon), via the explicit
	//     SMB dialer's WithSecurity, and
	//   - the NETLOGON sealed-schannel context that NewSecureChannelClient reads
	//     from cli.Conn().Context() to run ServerAuthenticate, via
	//     dcerpc.WithCredentials on the Dial (it lands in the connection's
	//     security context, which cli.Conn().Context() returns).
	// Supplying it on the dial gctx via gssapi.WithCredential is NOT sufficient:
	// dcerpc rebuilds the connection's security context, dropping the dial ctx's
	// context-local credential store, so the schannel falls back to the (empty)
	// global store and ServerAuthenticate fails with "defective credential".
	//
	// Domain must be set: the schannel NL_AUTH_MESSAGE carries it (Samba rejects a
	// schannel bind "without domain"), and the Kerberos SMB session uses it as the
	// machine principal's realm. The realm (DNS form) satisfies both — go-msrpc
	// sends it as the NL_AUTH DNSDomainName (#1345).
	machineCred := credential.NewFromPassword(
		mc.AccountName, mc.Password,
		credential.Workstation(mc.Workstation),
		credential.Domain(mc.Realm))

	// Samba rejects the sealed-schannel AlterContext over ncacn_ip_tcp with
	// RPC_S_UNKNOWN_AUTHN_SERVICE (0x721); the named-pipe transport (\PIPE\NETLOGON
	// over SMB) is the binding Samba accepts. That SMB session must authenticate
	// the machine account with Kerberos (Samba refuses machine-account NTLM over
	// SMB), targeting the DC's cifs/ SPN. Bind the netlogon pipe directly
	// (ncacn_np:[netlogon]) rather than via the endpoint mapper (#1345).
	dialer := smb2.NewDialer(smb2.WithSecurity(
		gssapi.WithTargetName(spn),
		gssapi.WithCredential(machineCred),
	))
	cc, err := dcerpc.Dial(gctx, server,
		dcerpc.WithEndpoint("ncacn_np:[netlogon]"),
		dcerpc.WithSMBDialer(dialer),
		dcerpc.WithCredentials(machineCred),
	)
	if err != nil {
		return fmt.Errorf("netlogon: dial %s: %w", server, err)
	}

	// Pass the machine credential to NewSecureChannelClient too, not just Dial:
	// it forwards these opts into the sealed-schannel AlterContext, whose NETLOGON
	// mechanism resolves the credential from the AlterContext's own security
	// context (a fresh one built from these opts). Without it, AlterContext falls
	// back to the empty process-global store and fails with "defective credential".
	cli, err := logon.NewSecureChannelClient(gctx, cc,
		dcerpc.WithSeal(),
		dcerpc.WithCredentials(machineCred),
	)
	if err != nil {
		_ = cc.Close(gctx)
		return fmt.Errorf("netlogon: secure channel client: %w", err)
	}

	sc.cc = cc
	sc.cli = cli

	// LogonServer / PrimaryName for the NETLOGON RPCs (samLogon, setPassword) is
	// derived LOCALLY from the Kerberos SPN (cifs/<dc-fqdn>) of the DC we dialed —
	// we deliberately do NOT issue DsrGetDcName over the sealed channel.
	//
	// Why not GetDCName: against a real Windows DC that call's response fails to
	// decode in go-msrpc AND the decode error tears down the shared DCERPC
	// transport, so the very next NetrLogonSamLogon on this channel fails with
	// "transport is closed" — the passthrough never validates the user (#1629). The
	// earlier comment that GetDCName failure is "non-fatal" held only against Samba
	// (which returned a decodable response); on Windows it is fatal to the channel.
	//
	// MS-NRPC treats LogonServer loosely: the DC authenticates the caller by the
	// secure-channel authenticator (established above), not by this string, and both
	// Windows and Samba accept the short DC label. So the local derivation is
	// sufficient and removes a fragile, inessential round-trip.
	sc.dcName = deriveLogonServer(spn, mc.DomainName)

	return nil
}

// deriveLogonServer builds the UNC "\\<DCNAME>" used as the NETLOGON LogonServer
// (NetrLogonSamLogon) / PrimaryName (NetrServerPasswordSet2). It takes the short
// host label of the DC's Kerberos SPN (cifs/<fqdn>), so a dialed cifs/dc01.example.com
// yields "\\DC01". When no host can be derived it falls back to the NetBIOS domain
// name.
//
// This assumes the DC's NetBIOS/computer name equals the uppercased leftmost DNS
// label — the AD default. It can diverge for a renamed DC or a DNS label longer
// than the 15-char NetBIOS limit; that is tolerated because the DC authenticates
// the caller by the secure-channel authenticator (established in connect), not by
// this string, which MS-NRPC treats as informational. Deriving it locally is what
// avoids the DsrGetDcName round-trip whose response decode tears down the shared
// DCERPC transport on a real Windows DC (#1629).
func deriveLogonServer(spn, domainName string) string {
	host := strings.TrimPrefix(spn, "cifs/")
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "\\\\" + domainName
	}
	return "\\\\" + strings.ToUpper(host)
}

// setPassword changes the machine account's password on the DC via
// NetrServerPasswordSet2 (MS-NRPC §3.5.4.4.6), reusing the already-established
// sealed secure channel. sc.mu is held for the full RPC so it serializes with
// samLogon and close/reset, exactly like samLogon.
//
// PasswordSet2 is NOT one of the methods the go-msrpc secure-channel client
// auto-wraps with the per-call NETLOGON authenticator dance, so we drive it
// manually: type-assert the client to the exported Encrypt/SetAuthenticators/
// VerifyAuthenticator method set, build the encrypted NL_TRUST_PASSWORD, fill
// the request authenticator, call, and verify the return authenticator.
func (sc *SecureChannel) setPassword(ctx context.Context, mc MachineCredential, newPassword string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.cli == nil {
		return fmt.Errorf("netlogon: channel not connected")
	}

	enc, ok := sc.cli.(encryptor)
	if !ok {
		// Defensive: the concrete go-msrpc secure-channel client always exposes
		// these methods; a future refactor that hides them would land here.
		return fmt.Errorf("netlogon: secure channel client does not expose authenticator/encrypt methods required for PasswordSet2")
	}

	trustPwd, err := buildTrustPassword(ctx, enc, newPassword)
	if err != nil {
		return err
	}

	// PrimaryName is the DC's UNC name, derived locally in connect()
	// (deriveLogonServer) and therefore always non-empty.
	req := &logon.PasswordSet2Request{
		PrimaryName:       sc.dcName,
		AccountName:       mc.AccountName,
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      mc.Workstation,
		ClearNewPassword:  trustPwd,
	}
	// Fill req.Authenticator (and allocate the holder for the return authenticator).
	var ret *logon.Authenticator
	if err := enc.SetAuthenticators(ctx, &req.Authenticator, &ret); err != nil {
		return fmt.Errorf("netlogon: PasswordSet2 authenticator: %w", err)
	}

	out, err := sc.cli.PasswordSet2(ctx, req)
	if err != nil {
		return fmt.Errorf("netlogon: PasswordSet2: %w", err)
	}
	if out.Return != 0 {
		return fmt.Errorf("netlogon: PasswordSet2 returned 0x%08x", uint32(out.Return))
	}
	// Verify the server's return authenticator to confirm the DC, not a MITM,
	// processed the call with our session key.
	if out.ReturnAuthenticator == nil || out.ReturnAuthenticator.Credential == nil {
		return fmt.Errorf("netlogon: PasswordSet2 missing return authenticator")
	}
	if err := enc.VerifyAuthenticator(ctx, out.ReturnAuthenticator); err != nil {
		return fmt.Errorf("netlogon: PasswordSet2 return authenticator verify: %w", err)
	}
	return nil
}

// close tears down the cached connection. It is self-locking (takes sc.mu) and
// idempotent, so it blocks until any in-flight samLogon on this channel returns
// before closing the connection — a teardown never races an in-flight RPC.
func (sc *SecureChannel) close(ctx context.Context) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.cc != nil {
		_ = sc.cc.Close(ctx)
		sc.cc = nil
	}
	sc.cli = nil
	sc.dcName = ""
}

// samLogon performs a NetrLogonSamLogon RPC call using the established channel.
// It is self-locking: sc.mu is held for the full duration of the RPC so that
// concurrent NetworkLogon calls are serialized (preserving the chained MS-NRPC
// sequence number) and a close cannot race an in-flight SAMLogon. It returns a
// "channel not connected" error if the channel was torn down (e.g. by a
// concurrent Reload) before this call acquired the lock; the caller rebuilds.
func (sc *SecureChannel) samLogon(ctx context.Context, mc MachineCredential, req NetworkLogonRequest) (*LogonResult, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.cli == nil {
		return nil, errChannelNotConnected
	}

	cli := sc.cli

	// LogonServer is the DC's UNC name (\\DC01) per MS-NRPC, derived locally in
	// connect() (deriveLogonServer) and therefore always non-empty.
	logonServer := sc.dcName

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
		// A transport-level RPC failure (not a DC-returned NTSTATUS) is a transient
		// channel condition, not a logon failure: a concurrent Reload can tear the
		// sealed SMB session down AFTER this call passed the sc.cli == nil guard but
		// while cli.SAMLogon is on the wire, yielding a broken-pipe/EOF rather than
		// the errChannelNotConnected sentinel. Wrap with errChannelNotConnected so
		// NetworkLogon rebuilds + retries (errors.Is sees the sentinel) instead of
		// failing the logon that a rebuild would have cleared. Unlike a credential
		// validation failure this can never amplify toward AD account lockout.
		return nil, fmt.Errorf("netlogon: SAMLogon: %w: %w", errChannelNotConnected, err)
	}

	if out.Return != 0 {
		return nil, &SamLogonStatusError{Status: uint32(out.Return)}
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
