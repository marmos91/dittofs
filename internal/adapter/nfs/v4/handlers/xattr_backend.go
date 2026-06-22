package handlers

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// XattrBackend resolves named (extended) attribute operations for the RFC 8276
// NFSv4.2 xattr handlers. It is deliberately a small interface owned by this
// package (dependency inversion) so PR2 does not depend on the concrete xattr
// methods landing in PR1 (#1285): *metadata.Service satisfies it structurally,
// and PR1's unified resolver will swap in transparently.
//
// All names passed across this interface are BARE store keys — the
// user-namespace name with no "user." prefix (canonicalizeXattrName strips any
// leading "user." before the handler calls this interface). This is the same
// key SMB extended attributes and named streams use, which is what makes a
// value set over one protocol visible over the other; the unified resolver
// (pkg/metadata/xattr.go) reads and writes these bare keys.
type XattrBackend interface {
	// GetXattr returns the value of the named xattr and whether it is present.
	GetXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string) ([]byte, bool, error)
	// SetXattr creates or replaces the named xattr (unconditional upsert;
	// create/replace semantics are enforced by the handler).
	SetXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string, value []byte) error
	// RemoveXattr deletes the named xattr.
	RemoveXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string) error
	// ListXattr returns the names of all xattrs on the file in bare form (no
	// "user." prefix); the LISTXATTRS handler sends them to the wire verbatim
	// and the client re-adds "user.".
	ListXattr(ctx *metadata.AuthContext, h metadata.FileHandle) ([]string, error)
}

// xattrBackendForHandler returns the xattr backend the handler should use. An
// explicitly injected backend (SetXattrBackend, used by tests) wins; otherwise
// the registry's *metadata.Service is used, which satisfies XattrBackend.
func xattrBackendForHandler(h *Handler) (XattrBackend, error) {
	if h.xattrBackend != nil {
		return h.xattrBackend, nil
	}
	svc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no metadata service configured for xattr backend")
	}
	return svc, nil
}

// xattrUserPrefix is the "user." namespace prefix as it appears on a Linux
// client. The NFSv4.2 wire name is already bare ("user.foo" on the client
// arrives as "foo"); the prefix is purely a client-side convention, never a
// storage concept. We strip a single leading "user." defensively for clients
// that send the namespaced form, but the canonical STORE key is the bare name.
const xattrUserPrefix = "user."

// canonicalizeXattrName maps a wire xattr name to its canonical STORE key — the
// bare user-namespace name, with no "user." prefix. This is the same key SMB
// extended attributes and named streams use, so a value set over one protocol
// is visible over the other (cross-protocol parity).
//
// It returns ok=false (caller → NFS4ERR_NOXATTR) for an empty name or a name in
// a reserved non-user namespace — one whose first dot-segment is "system",
// "trusted", or "security" (RFC 8276 §8.1: a server may reject names it does
// not support). Any other dotted name (e.g. "com.apple.metadata:…", "foo.bar")
// is a valid user xattr and is kept verbatim.
//
// A single leading "user." prefix is stripped (a client that sends the
// namespaced form converges on the same bare key as one that sends it bare);
// the bare prefix "user." with no key is rejected.
//
// No migration path is needed for the prior "user.<name>" storage form: #1285
// is unreleased, so no deployment has persisted NFS-written xattrs under the
// old prefixed key. SMB stores bare keys (cifs strips "user." on the wire and
// Windows EA names carry no "user." namespace), so a "user."-prefixed store key
// cannot arise in practice — hence GET/LIST/REMOVE all key the bare name with
// no dual-read fallback.
func canonicalizeXattrName(name string) (canonical string, ok bool) {
	if name == "" {
		return "", false
	}
	// Strip one leading "user." for clients that send the namespaced form.
	if strings.HasPrefix(name, xattrUserPrefix) {
		name = name[len(xattrUserPrefix):]
		if name == "" {
			return "", false
		}
	}
	if isReservedXattrNamespace(name) {
		return "", false
	}
	return name, true
}

// isReservedXattrNamespace reports whether a name belongs to a namespace DittoFS
// does not expose over NFSv4.2 — one whose first dot-segment is "system",
// "trusted", or "security". This both rejects such names on input and hides the
// reserved SMB EAs that live in the same inline backing (e.g. "security.NTACL")
// from LISTXATTRS, without relying on a storage-side prefix to mark membership.
func isReservedXattrNamespace(name string) bool {
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return false
	}
	switch name[:i] {
	case "system", "trusted", "security":
		return true
	}
	return false
}
