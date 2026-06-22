# SMB NTLM passthrough via NETLOGON (MS-NRPC) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let AD domain users authenticate over SMB NTLM by delegating the logon to a Domain Controller over the NETLOGON secure channel (MS-NRPC), instead of failing because there is no local NT hash.

**Architecture:** A new `internal/auth/netlogon` package wraps `github.com/oiweiwei/go-msrpc` to establish a sealed NETLOGON secure channel with a machine credential and call `NetrLogonSamLogonEx`. The SMB handler (`completeNTLMAuth`) gains a fallback: on a local-user miss it forwards the client's NTLM challenge/response to a DC via a small injected `NetlogonAuthenticator` interface, then reuses the existing #1317 directory-resolved-identity path to build the session. Machine credential + DC config ride on the existing kerberos identity-provider config (offline secret now; online-join is deferred to #1323).

**Tech Stack:** Go, `github.com/oiweiwei/go-msrpc` (DCE/RPC + schannel + `nrpc/logon`), existing DittoFS SMB/NTLM stack (`internal/adapter/smb/...`), GORM control-plane store, Cobra `dfsctl`, Samba AD-DC docker fixture (`test/integration/ad-dc`).

## Global Constraints

- **Sealed schannel mandatory:** use `dcerpc.WithSeal()`; `WithSign()` is rejected by post-CVE-2020-1472 DCs. Copy verbatim from the go-msrpc example.
- **Fail closed:** any passthrough error (not configured, DC unreachable, validation failure) → existing `STATUS_LOGON_FAILURE`. Never fall back to guest, never panic.
- **Local-first:** the existing local user + NT-hash path runs first and is unchanged. Passthrough is a fallback only on local miss.
- **Import-cycle rule (#1311):** `pkg/config` imports `controlplane/api`; SMB handlers MUST NOT import `pkg/config`. Handlers consume a plain DTO; `cmd/dfs` maps DTO → `config.KerberosConfig`.
- **Secret posture:** the machine secret is write-only over the API — redact it the same way the LDAP `bind_password` is redacted (`MarshalJSON` redacts; a `MarshalStored`-style path persists the real value).
- **Module path:** import packages under the repo module root (check `go.mod`; referenced internal paths in this plan are repo-relative).
- **Build tag for live AD tests:** Docker/DC-dependent tests use `//go:build ad_dc`, run by `.github/workflows/ad-dc.yml`.

---

## File structure

- `internal/auth/netlogon/credential.go` — `MachineCredential`, `MachineCredentialProvider` iface, `offlineCredential` impl.
- `internal/auth/netlogon/sid.go` — domain-SID + RID → SID string helpers.
- `internal/auth/netlogon/securechannel.go` — `SecureChannel`: cached, mutex-guarded go-msrpc schannel client; lazy connect + re-establish.
- `internal/auth/netlogon/authenticator.go` — `Authenticator` + `LogonResult`; `NetworkLogon(...)`; the `NetlogonAuthenticator` interface lives here for handler injection.
- `internal/auth/netlogon/*_test.go` — unit tests (sid, request-build, response-map) + `//go:build ad_dc` live test.
- `pkg/config/config.go` — add `MachineAccountConfig` to `KerberosConfig`.
- `internal/adapter/smb/handlers/...` — machine-account DTO + wiring; `completeNTLMAuth` fallback branch.
- `cmd/dfs/...` — DTO → config mapping; construct the `Authenticator` and inject into the SMB handler.
- `pkg/controlplane/models/identity_provider.go` + store + REST DTO — machine-account fields + redaction (rides on the kerberos config JSON).
- `cmd/dfsctl/...` — `identity-provider kerberos` machine-account flags.
- `test/integration/ad-dc/entrypoint.sh` + new `ad_dc`-tagged test — provision DittoFS machine account; member NTLM logon.
- `docs/SMB.md`, `docs/SECURITY.md` — flip the "not implemented" NTLM caveats.

---

## Task 1: Add go-msrpc dependency

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: the `github.com/oiweiwei/go-msrpc/...` packages become importable.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/oiweiwei/go-msrpc@latest`

- [ ] **Step 2: Write a compile-only import probe test**

Create `internal/auth/netlogon/dep_probe_test.go`:
```go
package netlogon

import (
	"testing"

	_ "github.com/oiweiwei/go-msrpc/dcerpc"
	_ "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	_ "github.com/oiweiwei/go-msrpc/ssp/credential"
	_ "github.com/oiweiwei/go-msrpc/ssp/gssapi"
	_ "github.com/oiweiwei/go-msrpc/ssp/ntlm"
)

func TestDepImportable(t *testing.T) {}
```

- [ ] **Step 3: Verify it builds**

Run: `go test ./internal/auth/netlogon/ -run TestDepImportable -v`
Expected: PASS (and `go.sum` updated).

- [ ] **Step 4: Tidy + commit**

```bash
go mod tidy
git add go.mod go.sum internal/auth/netlogon/dep_probe_test.go
git commit -S -m "build: add go-msrpc dependency for NETLOGON passthrough (#1314)"
```

---

## Task 2: SID construction helper

**Files:**
- Create: `internal/auth/netlogon/sid.go`
- Test: `internal/auth/netlogon/sid_test.go`

**Interfaces:**
- Produces: `func SIDFromRID(domainSID string, rid uint32) (string, error)` — appends a RID to a domain SID (`S-1-5-21-a-b-c` → `S-1-5-21-a-b-c-<rid>`). Validates the domain SID is well-formed (`S-1-...`, ≥3 components after `S-1`).

- [ ] **Step 1: Write the failing test**

```go
package netlogon

import "testing"

func TestSIDFromRID(t *testing.T) {
	got, err := SIDFromRID("S-1-5-21-1004336348-1177238915-682003330", 1103)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "S-1-5-21-1004336348-1177238915-682003330-1103"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSIDFromRIDRejectsMalformed(t *testing.T) {
	if _, err := SIDFromRID("not-a-sid", 1103); err == nil {
		t.Fatal("expected error for malformed domain SID")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/netlogon/ -run TestSIDFromRID -v`
Expected: FAIL (undefined `SIDFromRID`).

- [ ] **Step 3: Implement**

```go
package netlogon

import (
	"fmt"
	"strings"
)

// SIDFromRID appends a relative identifier to a domain SID.
func SIDFromRID(domainSID string, rid uint32) (string, error) {
	if !strings.HasPrefix(domainSID, "S-1-") {
		return "", fmt.Errorf("netlogon: malformed domain SID %q", domainSID)
	}
	if strings.Count(domainSID, "-") < 3 {
		return "", fmt.Errorf("netlogon: incomplete domain SID %q", domainSID)
	}
	return fmt.Sprintf("%s-%d", domainSID, rid), nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/netlogon/ -run TestSIDFromRID -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/netlogon/sid.go internal/auth/netlogon/sid_test.go
git commit -S -m "feat(netlogon): SID-from-RID helper (#1314)"
```

---

## Task 3: Machine credential provider

**Files:**
- Create: `internal/auth/netlogon/credential.go`
- Test: `internal/auth/netlogon/credential_test.go`

**Interfaces:**
- Produces:
  - `type MachineCredential struct { AccountName, Password, Workstation, DomainName, Realm string; DCAddresses []string }` (AccountName e.g. `DITTOFS$`; Workstation is the netbios host without `$`).
  - `type MachineCredentialProvider interface { Credential(ctx context.Context) (*MachineCredential, error) }`
  - `func NewOfflineProvider(c MachineCredential) MachineCredentialProvider` — returns a static provider; errors from `Credential` if `AccountName`/`Password`/`DomainName` empty or `DCAddresses` is empty.
- Consumes: nothing.

- [ ] **Step 1: Write the failing test**

```go
package netlogon

import (
	"context"
	"testing"
)

func TestOfflineProviderReturnsCredential(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{
		AccountName: "DITTOFS$", Password: "secret",
		Workstation: "DITTOFS", DomainName: "DITTOFS", Realm: "DITTOFS.AD",
		DCAddresses: []string{"dc1.dittofs.ad"},
	})
	got, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AccountName != "DITTOFS$" || len(got.DCAddresses) != 1 {
		t.Fatalf("unexpected credential: %+v", got)
	}
}

func TestOfflineProviderValidates(t *testing.T) {
	p := NewOfflineProvider(MachineCredential{AccountName: "DITTOFS$"})
	if _, err := p.Credential(context.Background()); err == nil {
		t.Fatal("expected validation error for incomplete credential")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/netlogon/ -run TestOfflineProvider -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement**

```go
package netlogon

import (
	"context"
	"errors"
)

type MachineCredential struct {
	AccountName string // e.g. "DITTOFS$"
	Password    string // machine password (offline secret)
	Workstation string // netbios host, no "$"
	DomainName  string // netbios domain, e.g. "DITTOFS"
	Realm       string // e.g. "DITTOFS.AD"
	DCAddresses []string
}

type MachineCredentialProvider interface {
	Credential(ctx context.Context) (*MachineCredential, error)
}

type offlineProvider struct{ c MachineCredential }

// NewOfflineProvider returns a provider backed by a statically configured secret.
func NewOfflineProvider(c MachineCredential) MachineCredentialProvider {
	return &offlineProvider{c: c}
}

func (p *offlineProvider) Credential(ctx context.Context) (*MachineCredential, error) {
	if p.c.AccountName == "" || p.c.Password == "" || p.c.DomainName == "" {
		return nil, errors.New("netlogon: incomplete machine credential (account/password/domain required)")
	}
	if len(p.c.DCAddresses) == 0 {
		return nil, errors.New("netlogon: no DC address configured")
	}
	cp := p.c
	return &cp, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/netlogon/ -run TestOfflineProvider -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/netlogon/credential.go internal/auth/netlogon/credential_test.go
git commit -S -m "feat(netlogon): machine-credential provider seam + offline impl (#1314)"
```

---

## Task 4: Authenticator + NetworkLogon (request build / response map)

This task makes the network-logon call mockable for the handler and unit-tests the pure mapping. The live schannel is exercised in Task 6.

**Files:**
- Create: `internal/auth/netlogon/authenticator.go`
- Test: `internal/auth/netlogon/authenticator_test.go`

**Interfaces:**
- Produces:
  - `type LogonResult struct { SessionBaseKey [16]byte; UserSID string; GroupSIDs []string; Username, DomainName string }`
  - `type NetlogonAuthenticator interface { NetworkLogon(ctx context.Context, req NetworkLogonRequest) (*LogonResult, error) }`
  - `type NetworkLogonRequest struct { Username, Domain string; ServerChallenge [8]byte; NTResponse, LMResponse []byte }`
  - `func samInfo4ToResult(domainSID string, userRID uint32, groupRIDs []uint32, sessionKey [16]byte, user, domain string) (*LogonResult, error)` — internal mapping (unit-tested directly), builds `UserSID` + `GroupSIDs` via `SIDFromRID`.
- Consumes: `SIDFromRID` (Task 2), `MachineCredentialProvider` (Task 3), `SecureChannel` (Task 5, injected).

- [ ] **Step 1: Write the failing test (pure mapping)**

```go
package netlogon

import "testing"

func TestSAMInfo4ToResult(t *testing.T) {
	var key [16]byte
	key[0] = 0xAB
	res, err := samInfo4ToResult(
		"S-1-5-21-1-2-3", 1103, []uint32{513, 1104}, key, "alice", "DITTOFS",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.UserSID != "S-1-5-21-1-2-3-1103" {
		t.Fatalf("user SID: %q", res.UserSID)
	}
	if len(res.GroupSIDs) != 2 || res.GroupSIDs[0] != "S-1-5-21-1-2-3-513" {
		t.Fatalf("group SIDs: %v", res.GroupSIDs)
	}
	if res.SessionBaseKey != key {
		t.Fatal("session key not propagated")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/netlogon/ -run TestSAMInfo4ToResult -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement the mapping + interfaces**

```go
package netlogon

import "context"

type NetworkLogonRequest struct {
	Username        string
	Domain          string
	ServerChallenge [8]byte // the challenge DittoFS sent in the NTLM Type-2
	NTResponse      []byte  // client's NtChallengeResponse
	LMResponse      []byte  // client's LmChallengeResponse (may be empty)
}

type LogonResult struct {
	SessionBaseKey [16]byte
	UserSID        string
	GroupSIDs      []string
	Username       string
	DomainName     string
}

type NetlogonAuthenticator interface {
	NetworkLogon(ctx context.Context, req NetworkLogonRequest) (*LogonResult, error)
}

func samInfo4ToResult(domainSID string, userRID uint32, groupRIDs []uint32, sessionKey [16]byte, user, domain string) (*LogonResult, error) {
	userSID, err := SIDFromRID(domainSID, userRID)
	if err != nil {
		return nil, err
	}
	groups := make([]string, 0, len(groupRIDs))
	for _, rid := range groupRIDs {
		sid, err := SIDFromRID(domainSID, rid)
		if err != nil {
			return nil, err
		}
		groups = append(groups, sid)
	}
	return &LogonResult{
		SessionBaseKey: sessionKey,
		UserSID:        userSID,
		GroupSIDs:      groups,
		Username:       user,
		DomainName:     domain,
	}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/netlogon/ -run TestSAMInfo4ToResult -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/netlogon/authenticator.go internal/auth/netlogon/authenticator_test.go
git commit -S -m "feat(netlogon): authenticator interface + SAMInfo4 mapping (#1314)"
```

---

## Task 5: SecureChannel + live Authenticator wiring

Wires the go-msrpc schannel client and implements `Authenticator.NetworkLogon` on top of it. The DC-touching path is verified live in Task 6; here we add the structure and a constructor-validation unit test.

**Files:**
- Create: `internal/auth/netlogon/securechannel.go`
- Modify: `internal/auth/netlogon/authenticator.go` (add `Authenticator` struct implementing `NetlogonAuthenticator`)
- Test: `internal/auth/netlogon/securechannel_test.go`

**Interfaces:**
- Produces:
  - `func NewAuthenticator(p MachineCredentialProvider) *Authenticator` — implements `NetlogonAuthenticator`.
  - `Authenticator.NetworkLogon(ctx, req)` — lazy-connects a cached `SecureChannel`, calls SAMLogon, maps the result; re-establishes the channel once on RPC error.
  - `Authenticator.Close()` — closes the cached connection.
- Consumes: `MachineCredentialProvider`, go-msrpc.

go-msrpc call shape (from the upstream example — keep verbatim where noted):
```go
import (
	"github.com/oiweiwei/go-msrpc/dcerpc"
	"github.com/oiweiwei/go-msrpc/ssp"
	"github.com/oiweiwei/go-msrpc/ssp/credential"
	"github.com/oiweiwei/go-msrpc/ssp/gssapi"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	"github.com/oiweiwei/go-msrpc/msrpc/epm/epm/v3"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
)

// connect (under mutex, cached):
cred := credential.NewFromPassword(mc.AccountName, mc.Password, credential.Workstation(mc.Workstation))
gctx := gssapi.NewSecurityContext(ctx)
gssapi.AddCredential(cred)        // register machine cred
gssapi.AddMechanism(ssp.Netlogon)
cc, err := dcerpc.Dial(gctx, server, epm.EndpointMapper(gctx, server))
cli, err := logon.NewSecureChannelClient(gctx, cc, dcerpc.WithSeal(), dcerpc.WithEndpoint("ncacn_ip_tcp:")) // WithSeal mandatory

// per logon:
out, err := cli.SAMLogon(gctx, &logon.SAMLogonRequest{
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
			LMChallengeResponse: &logon.String{Buffer: req.LMResponse},
			NTChallengeResponse: &logon.String{Buffer: req.NTResponse},
		},
	}},
	ValidationLevel: logon.ValidationInfoClassSAMInfo4,
})
// out.ValidationInformation -> SAMInfo4: UserID (RID), GroupIDs ([]GroupMembership{RelativeID}),
//   LogonDomainID (domain SID), UserSessionKey -> samInfo4ToResult(...)
```
> The exact field path from `out.ValidationInformation` to the SAMInfo4 struct (`UserID`, `GroupIDs`, `LogonDomainID`, `UserSessionKey`) must be read off the generated `logon` package types during implementation; map them into `samInfo4ToResult`. `LogonDomainID` renders to a SID string via the dtyp SID helper.

- [ ] **Step 1: Write the failing test (constructor + nil-cred guard)**

```go
package netlogon

import (
	"context"
	"testing"
)

func TestNetworkLogonRequiresCredential(t *testing.T) {
	a := NewAuthenticator(NewOfflineProvider(MachineCredential{})) // incomplete -> Credential() errors
	_, err := a.NetworkLogon(context.Background(), NetworkLogonRequest{
		Username: "alice", Domain: "DITTOFS",
	})
	if err == nil {
		t.Fatal("expected error when machine credential is incomplete")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/netlogon/ -run TestNetworkLogonRequiresCredential -v`
Expected: FAIL (undefined `NewAuthenticator`).

- [ ] **Step 3: Implement `securechannel.go` + `Authenticator`**

Create `securechannel.go` with a `SecureChannel` type holding `cc`, `cli`, the resolved `MachineCredential`, and a `sync.Mutex`; methods `connect(ctx)` (idempotent, builds the client per the shape above), `samLogon(ctx, req)`, `close()`. In `authenticator.go` add:
```go
type Authenticator struct {
	provider MachineCredentialProvider
	mu       sync.Mutex
	chan_    *SecureChannel
}

func NewAuthenticator(p MachineCredentialProvider) *Authenticator { return &Authenticator{provider: p} }

func (a *Authenticator) NetworkLogon(ctx context.Context, req NetworkLogonRequest) (*LogonResult, error) {
	mc, err := a.provider.Credential(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := a.ensureChannel(ctx, mc)
	if err != nil {
		return nil, err
	}
	res, err := sc.samLogon(ctx, *mc, req)
	if err != nil {
		// re-establish once on RPC error, then retry
		a.reset()
		if sc, err = a.ensureChannel(ctx, mc); err != nil {
			return nil, err
		}
		if res, err = sc.samLogon(ctx, *mc, req); err != nil {
			return nil, err
		}
	}
	return res, nil
}
```
Implement `ensureChannel` (lazy build under `a.mu`), `reset` (close + nil the cached channel), and `Close`.

- [ ] **Step 4: Run to verify it passes + package builds**

Run: `go test ./internal/auth/netlogon/ -v`
Expected: PASS (live SAMLogon not exercised here; constructor guard passes).

- [ ] **Step 5: Vet + commit**

```bash
go vet ./internal/auth/netlogon/
git add internal/auth/netlogon/securechannel.go internal/auth/netlogon/authenticator.go internal/auth/netlogon/securechannel_test.go
git commit -S -m "feat(netlogon): sealed secure-channel client + live SAMLogon (#1314)"
```

---

## Task 6: Live secure-channel integration test (ad_dc-gated)

**Files:**
- Modify: `test/integration/ad-dc/entrypoint.sh` (provision a DittoFS machine account + print its password)
- Create: `test/integration/ad-dc/netlogon_passthrough_test.go` (`//go:build ad_dc`)

**Interfaces:**
- Consumes: `netlogon.NewAuthenticator`, `netlogon.NewOfflineProvider`, `netlogon.NetworkLogonRequest`; the fixture's DC address + machine secret.

- [ ] **Step 1: Extend the fixture to create a machine account**

In `entrypoint.sh`, after the domain is provisioned, add (matching the file's existing style):
```bash
# Machine account for DittoFS NETLOGON passthrough tests.
samba-tool computer create dittofs --computerpassword="MachinePass01!" || true
```
(Confirm the account renders as `DITTOFS$`; adjust the create flags to the fixture's samba-tool version.)

- [ ] **Step 2: Write the failing live test**

```go
//go:build ad_dc

package addc

import (
	"context"
	"testing"

	"<module>/internal/auth/netlogon"
)

func TestNetlogonPassthroughAlice(t *testing.T) {
	dc := setupADDC(t) // existing fixture helper; exposes DC address
	a := netlogon.NewAuthenticator(netlogon.NewOfflineProvider(netlogon.MachineCredential{
		AccountName: "DITTOFS$", Password: "MachinePass01!",
		Workstation: "DITTOFS", DomainName: "DITTOFS", Realm: "DITTOFS.AD",
		DCAddresses: []string{dc.Address()},
	}))
	defer a.Close()

	// Compute alice's NTLMv2 response against a known 8-byte challenge using the
	// netlogon test helper (ssp/ntlm V2.ChallengeResponse), then:
	var challenge [8]byte // the same bytes used to compute the response
	res, err := a.NetworkLogon(context.Background(), netlogon.NetworkLogonRequest{
		Username: "alice", Domain: "DITTOFS",
		ServerChallenge: challenge, NTResponse: ntResp, LMResponse: lmResp,
	})
	if err != nil {
		t.Fatalf("passthrough failed: %v", err)
	}
	if res.UserSID == "" || len(res.GroupSIDs) == 0 {
		t.Fatalf("expected SIDs from DC, got %+v", res)
	}
}
```
Compute `ntResp`/`lmResp` with `ntlm.V2{}.ChallengeResponse` (same pattern as the go-msrpc example) keyed by `challenge`, using alice's password `TestPassword01!`.

- [ ] **Step 3: Run the gated test**

Run: `cd test/integration/ad-dc && go test -tags=ad_dc -timeout 20m -run TestNetlogonPassthrough .`
Expected: PASS — DC validates alice and returns her SID + group SIDs.

- [ ] **Step 4: Commit**

```bash
git add test/integration/ad-dc/entrypoint.sh test/integration/ad-dc/netlogon_passthrough_test.go
git commit -S -m "test(netlogon): live AD-DC passthrough for domain user (#1314)"
```

---

## Task 7: Config — `MachineAccountConfig` on `KerberosConfig`

**Files:**
- Modify: `pkg/config/config.go`
- Test: `pkg/config/config_test.go` (or the existing kerberos config test file)

**Interfaces:**
- Produces: `KerberosConfig.MachineAccount MachineAccountConfig` with fields:
  `Enabled bool`, `AccountName string`, `Secret string`, `KeytabPath string`, `DCAddresses []string` (yaml: `machine_account`, `enabled`, `account_name`, `secret`, `keytab_path`, `dc_address`).
- Consumes: nothing.

- [ ] **Step 1: Write the failing test**

```go
func TestKerberosMachineAccountParses(t *testing.T) {
	var k KerberosConfig
	k.MachineAccount = MachineAccountConfig{
		Enabled: true, AccountName: "DITTOFS$", Secret: "x",
		DCAddresses: []string{"dc1"},
	}
	if !k.MachineAccount.Enabled || k.MachineAccount.AccountName != "DITTOFS$" {
		t.Fatal("machine account fields not wired")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/config/ -run TestKerberosMachineAccount -v`
Expected: FAIL (undefined `MachineAccountConfig`).

- [ ] **Step 3: Implement**

Add to `pkg/config/config.go`:
```go
type MachineAccountConfig struct {
	Enabled     bool     `yaml:"enabled" json:"enabled"`
	AccountName string   `yaml:"account_name" json:"account_name"`
	Secret      string   `yaml:"secret" json:"secret"`
	KeytabPath  string   `yaml:"keytab_path" json:"keytab_path"`
	DCAddresses []string `yaml:"dc_address" json:"dc_address"`
}
```
Add `MachineAccount MachineAccountConfig \`yaml:"machine_account" json:"machine_account"\`` to `KerberosConfig`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./pkg/config/ -run TestKerberosMachineAccount -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -S -m "feat(config): kerberos machine-account sub-block for NETLOGON (#1314)"
```

---

## Task 8: Persist + redact machine-account fields in the identity-provider config

**Files:**
- Modify: `pkg/controlplane/models/identity_provider.go` (kerberos config DTO carried in the JSON `Config` blob)
- Modify: the REST DTO / redaction path that already redacts `bind_password` (find via `grep -rn "bind_password" pkg/controlplane`)
- Test: the existing identity-provider redaction test file

**Interfaces:**
- Produces: the kerberos provider's stored JSON gains `machine_account` with `secret`; `MarshalJSON` (API) redacts `secret`; the stored/`MarshalStored` path keeps the real value. Mirror exactly how `bind_password` is handled for LDAP.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing test (redaction)**

Model it on the existing LDAP `bind_password` redaction test: marshal a kerberos config with `machine_account.secret = "topsecret"` via the API path; assert the output contains the redaction sentinel (e.g. `"***"`) and not `topsecret`; assert the stored path round-trips the real secret.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/controlplane/... -run Redact -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the `machine_account` struct to the kerberos config DTO; in the redacting `MarshalJSON`, blank/sentinel the `secret` exactly as `bind_password`; in the stored marshaller, keep it. On PUT, if the incoming `secret` equals the sentinel, preserve the existing stored value (same write-only update semantics as `bind_password`).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./pkg/controlplane/... -run Redact -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/controlplane/...
git commit -S -m "feat(identity): persist+redact kerberos machine-account secret (#1314)"
```

---

## Task 9: dfsctl machine-account flags

**Files:**
- Modify: `cmd/dfsctl/...` (the `identity-provider kerberos` command; find via `grep -rn "identity-provider" cmd/dfsctl`)
- Modify: `docs/CLI.md` via generator (do NOT hand-edit)

**Interfaces:**
- Produces: `dfsctl identity-provider kerberos set` gains `--machine-account-enabled`, `--machine-account-name`, `--machine-secret`, `--machine-keytab`, `--dc-address` (repeatable). Maps to the REST DTO.

- [ ] **Step 1: Write the failing test**

Add a flag-wiring test alongside the existing kerberos command test (assert the new flags are registered and populate the request struct). If the command has no unit test, add a minimal one that builds the cobra command and checks `cmd.Flags().Lookup("machine-account-name") != nil`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/dfsctl/... -run Kerberos -v`
Expected: FAIL.

- [ ] **Step 3: Implement the flags + request mapping**

Wire the flags to the kerberos PUT request DTO. Treat `--machine-secret` as write-only (omit → keep existing).

- [ ] **Step 4: Run to verify it passes + regenerate docs**

Run: `go test ./cmd/dfsctl/... -run Kerberos -v` (PASS), then `go run ./cmd/gendocs`.

- [ ] **Step 5: Commit**

```bash
git add cmd/dfsctl/... docs/CLI.md
git commit -S -m "feat(dfsctl): kerberos machine-account flags for NETLOGON (#1314)"
```

---

## Task 10: Map DTO → config and construct the Authenticator in cmd/dfs

**Files:**
- Modify: `cmd/dfs/...` (where `KerberosConfig` is loaded and the SMB handler is constructed; find via `grep -rn "KerberosConfig\|NewProvider" cmd/dfs`)
- Modify: the SMB handler constructor to accept a `netlogon.NetlogonAuthenticator` (nil when disabled)
- Test: `cmd/dfs/...` mapping test

**Interfaces:**
- Produces: when `cfg.Kerberos.MachineAccount.Enabled`, build `netlogon.NewAuthenticator(netlogon.NewOfflineProvider(...))` from config (mapping `Secret`→`Password`, deriving `Workstation` from netbios host, `DomainName`/`Realm` from the kerberos config) and inject it into the SMB handler. When disabled, inject `nil`.
- Consumes: Task 5 `NewAuthenticator`, Task 7 config.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildNetlogonAuthenticatorFromConfig(t *testing.T) {
	k := config.KerberosConfig{
		Realm: "DITTOFS.AD", NetBIOSDomain: "DITTOFS",
		MachineAccount: config.MachineAccountConfig{
			Enabled: true, AccountName: "DITTOFS$", Secret: "s",
			DCAddresses: []string{"dc1"},
		},
	}
	a := buildNetlogonAuthenticator(k) // unit under test
	if a == nil {
		t.Fatal("expected authenticator when machine account enabled")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/dfs/... -run BuildNetlogon -v`
Expected: FAIL.

- [ ] **Step 3: Implement `buildNetlogonAuthenticator` + injection**

Map config → `MachineCredential`; return `nil` when `!Enabled`. Thread the result into the SMB handler constructor (add the parameter; pass `nil` at all other call sites). Respect the import-cycle rule — this mapping lives in `cmd/dfs`, not in handlers.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/dfs/... -run BuildNetlogon -v` and `go build ./...`
Expected: PASS + build clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/dfs/... internal/adapter/smb/...
git commit -S -m "feat(dfs): construct+inject NETLOGON authenticator from config (#1314)"
```

---

## Task 11: completeNTLMAuth fallback branch

**Files:**
- Modify: `internal/adapter/smb/handlers/session_setup.go` (`completeNTLMAuth`, ~line 1093) + the handler struct to hold the injected `netlogon.NetlogonAuthenticator`
- Test: `internal/adapter/smb/handlers/session_setup_netlogon_test.go`

**Interfaces:**
- Consumes: `netlogon.NetlogonAuthenticator`, the existing `synthUserFromResolved` / resolved-identity path (#1317), `CreateSessionWithUser`, `SetPACIdentity`, `configureSessionSigningWithKey`, `auth.DeriveSigningKey`.
- Produces: on local miss + passthrough success, a fully-built domain-user session.

- [ ] **Step 1: Write the failing test with a mock authenticator**

```go
package handlers

import (
	"context"
	"testing"

	"<module>/internal/auth/netlogon"
)

type fakeNetlogon struct{ res *netlogon.LogonResult; err error }

func (f *fakeNetlogon) NetworkLogon(ctx context.Context, req netlogon.NetworkLogonRequest) (*netlogon.LogonResult, error) {
	return f.res, f.err
}

func TestCompleteNTLMAuth_DomainUserPassthrough(t *testing.T) {
	// Arrange a handler whose local userStore.GetUser misses for "alice",
	// inject fakeNetlogon returning a LogonResult with SID + group SIDs +
	// a non-zero SessionBaseKey, and a stub resolver that resolves the SID to uid=10001.
	// Act: drive completeNTLMAuth with a domain NTLM Authenticate message.
	// Assert: a session is created for uid 10001, PAC SIDs set from the result,
	// and signing configured from the DC SessionBaseKey (not STATUS_LOGON_FAILURE).
	t.Skip("fill in with the package's existing handler test harness")
}
```
Replace the `t.Skip` body using the harness already used by other `handlers` tests (look at how Kerberos/NTLM handler tests build `SMBHandlerContext`, `PendingAuth`, and a fake user store).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/adapter/smb/handlers/ -run DomainUserPassthrough -v`
Expected: FAIL.

- [ ] **Step 3: Implement the fallback branch**

In `completeNTLMAuth`, after the local lookup misses (and before returning `STATUS_LOGON_FAILURE`), if `h.netlogon != nil`:
```go
res, err := h.netlogon.NetworkLogon(ctx.Context, netlogon.NetworkLogonRequest{
	Username:        authMsg.Username,
	Domain:          authMsg.Domain,
	ServerChallenge: pending.ServerChallenge,
	NTResponse:      authMsg.NtChallengeResponse,
	LMResponse:      authMsg.LmChallengeResponse,
})
if err != nil {
	// log at Debug; fall through to STATUS_LOGON_FAILURE (fail closed)
} else {
	// resolve UID/GID from res.UserSID via the existing resolver (synthUserFromResolved path),
	// build the session, SetPACIdentity(res.GroupSIDs, res.UserSID),
	// derive signing from res.SessionBaseKey via auth.DeriveSigningKey + configureSessionSigningWithKey.
}
```
Reuse the #1317 resolved-identity → synthetic-user code path that the Kerberos handler already uses when `userStore.GetUser` misses but the directory resolves the principal. Build the identity `Credential` with `ExternalID = res.UserSID` (and/or `username@domain`).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/adapter/smb/handlers/ -run DomainUserPassthrough -v`
Expected: PASS.

- [ ] **Step 5: Full package tests + commit**

```bash
go test ./internal/adapter/smb/...
git add internal/adapter/smb/handlers/...
git commit -S -m "feat(smb): NTLM domain-user fallback via NETLOGON passthrough (#1314)"
```

---

## Task 12: E2E acceptance — smbclient NTLM as domain user (ad_dc-gated)

**Files:**
- Create/modify: an `//go:build ad_dc` e2e test that starts DittoFS configured with the fixture machine account and runs `smbclient -m SMB3` forcing NTLM as `DITTOFS\alice` (find the pattern in existing `test/e2e/smb_*` or `test/integration/ad-dc` tests).

**Interfaces:**
- Consumes: the whole stack (Tasks 1-11), the AD-DC fixture, a DittoFS share.

- [ ] **Step 1: Write the failing e2e test**

Start DittoFS with `kerberos.machine_account` pointing at the fixture DC + `DITTOFS$`/`MachinePass01!`; create a share; run:
```
smbclient //127.0.0.1/demo -U 'DITTOFS\alice%TestPassword01!' -m SMB3 --option='client use kerberos=no' -c 'ls'
```
Assert exit 0 and a successful listing (no `NT_STATUS_LOGON_FAILURE`).

- [ ] **Step 2: Run it**

Run: `cd test/integration/ad-dc && go test -tags=ad_dc -timeout 20m -run TestSMBNTLMDomainUser .`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/...
git commit -S -m "test(e2e): smbclient NTLM domain-user login via NETLOGON (#1314)"
```

---

## Task 13: Docs — flip the NTLM caveats

**Files:**
- Modify: `docs/SMB.md` (the `### NTLM Authentication` note ~line 633 and `### NTLM Fallback` ~line 1166)
- Modify: `docs/SECURITY.md` (`### Guest Sessions and NTLM Fallback`, the "NTLM fallback occurs when" block)

**Interfaces:** docs only.

- [ ] **Step 1: Update `docs/SMB.md`**

In the `NTLM Authentication` note, replace the "not implemented" paragraph: NTLM now authenticates **local** users directly **and AD domain users via NETLOGON passthrough** when a machine account is configured (`kerberos.machine_account`); without it, domain NTLM still fails with `STATUS_LOGON_FAILURE`. State the offline-credential requirement and link the deferred trackers (#1323/#1324/#1325). Update the `### NTLM Fallback` section's step 4 to note domain users are validated by the DC, not the local hash.

- [ ] **Step 2: Update `docs/SECURITY.md`**

In the NTLM-fallback subsection, add that domain-user NTLM is validated by the DC over a **sealed** NETLOGON secure channel (no local hash), and note the machine-secret at-rest sensitivity.

- [ ] **Step 3: Commit**

```bash
git add docs/SMB.md docs/SECURITY.md
git commit -S -m "docs: SMB NTLM passthrough for AD domain users (#1314)"
```

---

## Self-review notes

- **Spec coverage:** package (T2-T5), live DC (T6), config (T7), persist/redact (T8), dfsctl (T9), wiring (T10), handler integration (T11), e2e (T12), docs (T13), dependency (T1). Deferred items (#1323/#1324/#1325) are intentionally out of scope.
- **Mockability:** the handler depends on `netlogon.NetlogonAuthenticator` (interface), so T11 unit-tests without a DC; live behaviour is covered by the ad_dc-gated T6/T12.
- **Type consistency:** `NetlogonAuthenticator.NetworkLogon(ctx, NetworkLogonRequest) (*LogonResult, error)` is used identically in T4 (def), T5 (impl), T10 (construct), T11 (consume). `MachineCredential`/`MachineAccountConfig` field names match across T3/T7/T10.
- **Library-specific risk:** the exact `out.ValidationInformation` → SAMInfo4 field path (T5) and samba-tool machine-account flags (T6) are the two spots requiring the implementer to confirm against the live library/fixture; both are isolated to one task each.
```