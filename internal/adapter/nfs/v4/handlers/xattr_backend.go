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
// All names passed across this interface are canonical store names (the
// "user."-prefixed form — see canonicalizeXattrName). This is the namespace
// the unified resolver (pkg/metadata/xattr.go) reads and writes; end-to-end
// cross-protocol name parity with SMB EAs/streams is exercised in PR3, not
// asserted here.
type XattrBackend interface {
	// GetXattr returns the value of the named xattr and whether it is present.
	GetXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string) ([]byte, bool, error)
	// SetXattr creates or replaces the named xattr (unconditional upsert;
	// create/replace semantics are enforced by the handler).
	SetXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string, value []byte) error
	// RemoveXattr deletes the named xattr.
	RemoveXattr(ctx *metadata.AuthContext, h metadata.FileHandle, name string) error
	// ListXattr returns the names of all xattrs on the file (store-canonical
	// form, i.e. including the "user." prefix).
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

// xattrUserPrefix is the only xattr namespace DittoFS exposes over NFSv4.2.
//
// The Linux NFS client carries only the "user." namespace on the wire and
// strips the prefix before sending the name (so a client setfattr -n user.foo
// arrives as "foo"). To keep names aligned with the SMB EA namespace, NFS
// canonicalizes incoming names to "user.<name>" before the backend and strips
// the prefix on the way out. Requests for any other namespace (system., trusted.,
// security., …) are rejected with NFS4ERR_NOXATTR (RFC 8276 §8.1: a server may
// reject names it does not support).
const xattrUserPrefix = "user."

// canonicalizeXattrName maps a wire xattr name to its store-canonical form
// ("user.<name>"). It returns ok=false (caller → NFS4ERR_NOXATTR) only for a
// name in a reserved non-user namespace — one whose first dot-segment is
// "system", "trusted", or "security". Any other bare name is placed in the
// user namespace, INCLUDING dotted names like "foo.bar" (a valid user xattr,
// e.g. "com.apple.metadata:…"), which become "user.foo.bar".
//
// The Linux client strips "user." so names usually arrive bare; a name that
// already carries the "user." prefix is accepted as-is (the bare prefix
// "user." with no key is rejected).
func canonicalizeXattrName(name string) (canonical string, ok bool) {
	if name == "" {
		return "", false
	}
	// Already namespaced as user.*: accept verbatim, but reject the bare prefix
	// with no key component ("user.") — that would be an empty on-wire name.
	if strings.HasPrefix(name, xattrUserPrefix) {
		if name == xattrUserPrefix {
			return "", false
		}
		return name, true
	}
	// A bare name in another namespace (system.*, trusted.*, security.*, …) is
	// rejected. We treat any dotted name whose first segment is a known reserved
	// namespace as unsupported; a bare name with no namespace defaults to user.
	if i := strings.IndexByte(name, '.'); i >= 0 {
		switch name[:i] {
		case "system", "trusted", "security":
			return "", false
		}
	}
	return xattrUserPrefix + name, true
}

// stripXattrPrefix reverses canonicalizeXattrName for names returned to the
// client: it drops the "user." prefix so the wire name matches what the Linux
// client sent. Names without the prefix are returned unchanged.
func stripXattrPrefix(name string) string {
	return strings.TrimPrefix(name, xattrUserPrefix)
}
