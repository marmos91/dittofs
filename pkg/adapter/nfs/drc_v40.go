package nfs

import (
	"bytes"

	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// maxDupReqBytes bounds both the request a v4.0 COMPOUND may present and the
// reply the duplicate request cache will keep for it.
//
// The cache exists for namespace mutations, and a CREATE, REMOVE, RENAME or
// LINK compound is a filehandle and a name or two -- hundreds of bytes, with a
// reply smaller still. Bulk-data compounds are far above this, and paying for
// them is pure cost: keying the cache checksums the whole request body, so a
// 1 MiB WRITE would buy nothing and pay for a checksum of every byte, twice,
// once to reserve the slot and once to release it. Capping the reply likewise
// keeps the cache bounded in bytes and not only in entries.
//
// ponytail: one bound for both directions, chosen to sit far above every
// namespace-mutating compound rather than tuned. A compound above it is simply
// left to re-execute on a retransmission, which is what the server did before
// the cache covered v4.0 at all. Split it in two, or raise it, only if a real
// workload is found whose mutations do not fit.
const maxDupReqBytes = 8 << 10

// drcEligibleV40Compound reports whether a COMPOUND should be tracked in the
// duplicate request cache: it must be v4.0, which has no session slot table of
// its own, and small enough that tracking it is worth what keying it costs.
// The size test comes first because it is the cheap one.
func drcEligibleV40Compound(data []byte) bool {
	return len(data) <= maxDupReqBytes && isV40Compound(data)
}

// drcRecordableReply reports whether a finished COMPOUND's reply should be kept
// for a retransmission to be answered from. The dispatcher decides whether the
// COMPOUND ran anything that must not run twice; this adds the size bound, which
// is what keeps a mutation bundled with a large read from pinning its whole
// reply in the cache.
func drcRecordableReply(cacheReply bool, replyLen int) bool {
	return cacheReply && replyLen <= maxDupReqBytes
}

// isV40Compound reports whether a COMPOUND request body declares minorversion 0.
//
// COMPOUND4args opens with the tag the server echoes back, a utf8str_cs whose
// wire encoding is that of a variable-length opaque, followed by the
// minorversion -- so the dialect is two fields into the body and is readable
// without decoding any operation. A body too malformed to yield them is left to
// ProcessCompound to reject.
func isV40Compound(data []byte) bool {
	reader := bytes.NewReader(data)
	if _, err := xdr.DecodeOpaque(reader); err != nil {
		return false
	}
	minorVersion, err := xdr.DecodeUint32(reader)
	return err == nil && minorVersion == v4types.NFS4_MINOR_VERSION_0
}
