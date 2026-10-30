// Package admit is admission's steps 2 and 3 for one call reaching one
// share: its security minimums and its client rules. It reads nothing but
// what it is handed and the netgroups it names, so it
// decides the same for a call whatever path brought it to the share.
package admit

import (
	"context"
	"net/netip"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// Netgroups is the one read admission makes beyond the share: a named
// netgroup's members, each a host name, an address or a CIDR.
type Netgroups interface {
	Members(ctx context.Context, name string) (members []string, found bool, err error)
}

// Check runs admission's steps 2 and 3 for a call by id reaching a share
// under policy p, in order, and answers whether a client rule narrowed the
// caller to read-only. The share's state, step 1, is its own Gate, which the
// caller runs first. The minimums are compared as stored: Kerberos only is
// ProofTicket, which an NTLM sign-in does not meet.
func Check(ctx context.Context, p entity.ExportPolicy, id vfs.Identity, ng Netgroups) (readOnly bool, err error) {
	if id.Proof < vfs.ProofKind(p.MinProof) || id.Protection < vfs.Protection(p.MinProtection) {
		return false, vfs.ErrWrongSecurity
	}
	if len(p.Clients) == 0 {
		return false, nil
	}
	addr := id.Client.Unmap()
	for _, r := range p.Clients {
		hit, err := matches(ctx, r, addr, ng)
		if err != nil {
			return false, err
		}
		if !hit {
			continue
		}
		if r.Access == entity.AccessNone {
			return false, vfs.ErrAccess
		}
		return r.Access == entity.AccessRead, nil
	}
	return false, vfs.ErrAccess
}

// matches reports whether rule r applies to a client at addr. A netgroup
// that does not exist fails closed: an admitting rule matches nobody, and a
// refusing rule everybody, so deleting a netgroup an early refusal names
// cannot let its hosts reach a later, broader rule.
//
// decision: a netgroup member that is a host name is never resolved, since
// the filesystem makes no network call: it matches every client under a
// refusing rule and none under an admitting one, the side that fails closed.
// A host name in an admitting rule therefore admits nobody. Resolve host
// names, in the node and handed in with the members, if an installation needs
// to admit clients by name.
func matches(ctx context.Context, r entity.ClientRule, addr netip.Addr, ng Netgroups) (bool, error) {
	if r.Match.Netgroup == "" {
		return addr.IsValid() && unmapped(r.Match.CIDR).Contains(addr), nil
	}
	members, found, err := ng.Members(ctx, r.Match.Netgroup)
	if err != nil {
		return false, err
	}
	if !found {
		return r.Access == entity.AccessNone, nil
	}
	for _, m := range members {
		if a, err := netip.ParseAddr(m); err == nil {
			if addr.IsValid() && a.Unmap() == addr {
				return true, nil
			}
		} else if p, err := netip.ParsePrefix(m); err == nil {
			if addr.IsValid() && unmapped(p).Contains(addr) {
				return true, nil
			}
		} else if r.Access == entity.AccessNone {
			return true, nil
		}
	}
	return false, nil
}

// unmapped is p with an IPv4-mapped IPv6 prefix as the IPv4 prefix it
// spans, as a client's address is unmapped before any rule compares it.
func unmapped(p netip.Prefix) netip.Prefix {
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p
}
