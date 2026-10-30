package admit

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

type groups map[string][]string

func (g groups) Members(_ context.Context, name string) ([]string, bool, error) {
	m, ok := g[name]
	return m, ok, nil
}

var (
	lab    = netip.MustParseAddr("10.0.0.5")
	office = netip.MustParseAddr("192.168.1.7")
	ng     = groups{"lab": {"10.0.0.5", "build01.example"}, "office": {"192.168.1.0/24"}, "mapped": {"::ffff:192.168.1.0/120"}}
)

func id(at netip.Addr, p vfs.ProofKind, prot vfs.Protection) vfs.Identity {
	return vfs.Identity{Proof: p, Protection: prot, Client: at}
}

// Security is checked before client rules, each refusing with its own
// error.
func TestCheckRunsItsStepsInOrder(t *testing.T) {
	ctx := context.Background()
	refuseAll := []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("0.0.0.0/0")}, Access: entity.AccessNone}}
	krb5i := entity.ExportPolicy{MinProof: entity.ProofTicket, MinProtection: entity.ProtectionIntegrity}
	for _, c := range []struct {
		name string
		p    entity.ExportPolicy
		id   vfs.Identity
		want error
	}{
		{"proof below", entity.ExportPolicy{MinProof: entity.ProofSecret, Clients: refuseAll}, id(lab, vfs.ProofClaimed, 0), vfs.ErrWrongSecurity},
		{"protection below", krb5i, id(lab, vfs.ProofTicket, vfs.ProtectionNone), vfs.ErrWrongSecurity},
		{"both met", krb5i, id(lab, vfs.ProofTicket, vfs.ProtectionPrivacy), nil},
		{"rule refuses", entity.ExportPolicy{Clients: refuseAll}, id(lab, vfs.ProofTicket, 0), vfs.ErrAccess},
	} {
		_, err := Check(ctx, c.p, c.id, ng)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// The first rule matching the client decides; none matching refuses; a
// missing netgroup and a host-name member each fail closed.
func TestClientRules(t *testing.T) {
	ctx := context.Background()
	on := func(rules ...entity.ClientRule) entity.ExportPolicy { return entity.ExportPolicy{Clients: rules} }
	cidr := func(p string, a entity.ShareAccess) entity.ClientRule {
		return entity.ClientRule{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix(p)}, Access: a}
	}
	group := func(n string, a entity.ShareAccess) entity.ClientRule {
		return entity.ClientRule{Match: entity.ClientMatch{Netgroup: n}, Access: a}
	}
	for _, c := range []struct {
		name     string
		p        entity.ExportPolicy
		at       netip.Addr
		refused  bool
		readOnly bool
	}{
		{"no rules", on(), lab, false, false},
		{"first match read", on(cidr("10.0.0.0/8", entity.AccessRead), cidr("10.0.0.0/8", entity.AccessReadWrite)), lab, false, true},
		{"first match read-write", on(cidr("10.0.0.0/8", entity.AccessReadWrite), cidr("10.0.0.0/8", entity.AccessRead)), lab, false, false},
		{"refused before a wider admit", on(cidr("10.0.0.5/32", entity.AccessNone), cidr("0.0.0.0/0", entity.AccessReadWrite)), lab, true, false},
		{"no match", on(cidr("10.0.0.0/8", entity.AccessReadWrite)), office, true, false},
		{"mapped address", on(cidr("10.0.0.0/8", entity.AccessReadWrite)), netip.AddrFrom16(lab.As16()), false, false},
		{"mapped cidr rule", on(cidr("::ffff:10.0.0.0/104", entity.AccessReadWrite)), lab, false, false},
		{"netgroup mapped cidr", on(group("mapped", entity.AccessReadWrite)), office, false, false},
		{"no address", on(cidr("0.0.0.0/0", entity.AccessReadWrite)), netip.Addr{}, true, false},
		{"netgroup address", on(group("lab", entity.AccessRead)), lab, false, true},
		{"netgroup cidr", on(group("office", entity.AccessReadWrite)), office, false, false},
		{"outside the netgroup", on(group("lab", entity.AccessReadWrite)), office, true, false},
		{"missing netgroup admits nobody", on(group("gone", entity.AccessReadWrite)), lab, true, false},
		{"missing netgroup refuses everybody", on(group("gone", entity.AccessNone), cidr("0.0.0.0/0", entity.AccessReadWrite)), office, true, false},
		{"host name refuses everybody", on(group("lab", entity.AccessNone), cidr("0.0.0.0/0", entity.AccessReadWrite)), office, true, false},
	} {
		ro, err := Check(ctx, c.p, id(c.at, vfs.ProofNone, 0), ng)
		if c.refused != errors.Is(err, vfs.ErrAccess) || ro != c.readOnly {
			t.Errorf("%s: readOnly %v, %v; want refused %v, readOnly %v", c.name, ro, err, c.refused, c.readOnly)
		}
	}
}

// nobodyHome holds no protocol ID and no membership, and every user it is
// asked about is live with a read-write grant.
type nobodyHome struct{}

func (nobodyHome) ByID(context.Context, entity.ProtocolScheme, uint32) (entity.Principal, bool, error) {
	return entity.Principal{}, false, nil
}
func (nobodyHome) User(context.Context, entity.Principal) (entity.User, bool, error) {
	return entity.User{}, true, nil
}
func (nobodyHome) Grant(context.Context, entity.ShareID, entity.Principal) (entity.ShareAccess, error) {
	return entity.AccessReadWrite, nil
}
func (nobodyHome) Groups(context.Context, entity.Principal) ([]entity.Principal, error) {
	return nil, nil
}

// A policy naming no anonymous principal refuses every caller it would
// make anonymous, rather than letting it act as the zero principal.
func TestNoAnonymousPrincipalFailsClosed(t *testing.T) {
	ctx := context.Background()
	share := entity.NewShareID()
	for _, c := range []struct {
		name string
		id   vfs.Identity
		p    entity.ExportPolicy
	}{
		{"squashed", vfs.Identity{Proof: vfs.ProofTicket, Evidence: vfs.Evidence{Principal: entity.NewUser()}}, entity.ExportPolicy{Squash: entity.SquashAll}},
		{"unmapped", vfs.Identity{Proof: vfs.ProofClaimed, Claims: vfs.Claims{UID: 1000}}, entity.ExportPolicy{}},
		{"proving nothing", vfs.Identity{}, entity.ExportPolicy{}},
	} {
		if got, _, err := Resolve(ctx, nobodyHome{}, c.id, share, c.p, entity.NotNarrowed); !errors.Is(err, vfs.ErrAccess) {
			t.Errorf("%s: acts as %v, %v; want ErrAccess", c.name, got.Principal, err)
		}
	}
}
