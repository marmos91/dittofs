package netlogon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/oiweiwei/go-msrpc/dcerpc"
	epm "github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/gssapi"
)

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

	cred := credential.NewFromPassword(mc.AccountName, mc.Password, credential.Workstation(mc.Workstation))
	gctx := gssapi.NewSecurityContext(ctx)
	gssapi.AddCredential(cred)
	gssapi.AddMechanism(ssp.Netlogon)

	cc, err := dcerpc.Dial(gctx, server, epm.EndpointMapper(gctx, server))
	if err != nil {
		return fmt.Errorf("netlogon: dial %s: %w", server, err)
	}

	cli, err := logon.NewSecureChannelClient(gctx, cc, dcerpc.WithSeal(), dcerpc.WithEndpoint("ncacn_ip_tcp:"))
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
func (sc *SecureChannel) samLogon(ctx context.Context, mc MachineCredential, req NetworkLogonRequest) (*LogonResult, error) {
	sc.mu.Lock()
	cli := sc.cli
	dcName := sc.dcName
	sc.mu.Unlock()

	if cli == nil {
		return nil, fmt.Errorf("netlogon: channel not connected")
	}

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
