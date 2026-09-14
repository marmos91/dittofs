package handlers

import (
	"encoding/binary"
	"runtime"
	"testing"
)

// declaredPathLen is the path length the crafted requests claim. Large enough
// that allocating it dwarfs the budget below, small enough that a regression
// costs the test run 64 MiB rather than 2 GiB.
const declaredPathLen = 64 << 20

// allocBudget bounds what decoding an 8-byte request may allocate. Decoding a
// real path allocates on the order of its length, so the budget leaves three
// orders of magnitude of headroom and still fails hard if the decoder sizes a
// buffer from the declared length instead of the delivered bytes.
const allocBudget = 8 << 20

// pathOverrunRequest is an XDR string header claiming declaredPathLen bytes of
// path that were never sent. Nothing follows the length, so a decoder that
// trusts it reads past the end of everything the peer delivered.
func pathOverrunRequest() []byte {
	msg := make([]byte, 4)
	binary.BigEndian.PutUint32(msg, declaredPathLen)
	return msg
}

// xdrString encodes a Go string as an XDR variable-length string: length, then
// the bytes, then padding to a four-byte boundary.
func xdrString(s string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(s)))
	out = append(out, s...)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestDecodeMountRequest_PathLengthDoesNotSizeAllocation pins the same rule on
// the MOUNT path that the RPC header decode already carries: a declared length
// never sizes an allocation on its own. MNT is reachable by any peer that can
// reach the mount service, so a request that allocates from its own length
// field is a lever on server memory.
func TestDecodeMountRequest_PathLengthDoesNotSizeAllocation(t *testing.T) {
	msg := pathOverrunRequest()

	var err error
	allocated := allocatedBy(func() { _, err = DecodeMountRequest(msg) })

	if err == nil {
		t.Fatal("DecodeMountRequest accepted a request whose path was never sent")
	}
	if allocated > allocBudget {
		t.Errorf("DecodeMountRequest allocated %d bytes decoding a %d-byte request (budget %d): "+
			"the declared path length sized the buffer, not the delivered bytes",
			allocated, len(msg), allocBudget)
	}
}

// TestDecodeUmountRequest_PathLengthDoesNotSizeAllocation is the UMNT twin.
// Both decoders were fixed together, so both are pinned together — a later
// change to one cannot silently regress while the other stays covered.
func TestDecodeUmountRequest_PathLengthDoesNotSizeAllocation(t *testing.T) {
	msg := pathOverrunRequest()

	var err error
	allocated := allocatedBy(func() { _, err = DecodeUmountRequest(msg) })

	if err == nil {
		t.Fatal("DecodeUmountRequest accepted a request whose path was never sent")
	}
	if allocated > allocBudget {
		t.Errorf("DecodeUmountRequest allocated %d bytes decoding a %d-byte request (budget %d): "+
			"the declared path length sized the buffer, not the delivered bytes",
			allocated, len(msg), allocBudget)
	}
}

// TestDecodeRequests_WellFormedPathStillDecodes guards the other direction:
// capping reads at the delivered length must not reject a legitimate request,
// whose path is by definition among the bytes it carried. Without this, the two
// tests above would pass against a decoder that rejected everything.
func TestDecodeRequests_WellFormedPathStillDecodes(t *testing.T) {
	const path = "/export"
	msg := xdrString(path)

	mnt, err := DecodeMountRequest(msg)
	if err != nil {
		t.Fatalf("DecodeMountRequest rejected a well-formed request: %v", err)
	}
	if mnt.DirPath != path {
		t.Errorf("MNT DirPath = %q, want %q", mnt.DirPath, path)
	}

	umnt, err := DecodeUmountRequest(msg)
	if err != nil {
		t.Fatalf("DecodeUmountRequest rejected a well-formed request: %v", err)
	}
	if umnt.DirPath != path {
		t.Errorf("UMNT DirPath = %q, want %q", umnt.DirPath, path)
	}
}
