package rpc

import (
	"encoding/binary"
	"runtime"
	"testing"
)

// declaredCredLen is the credential body length the crafted call claims. It is
// large enough that allocating it dwarfs the allocation budget below, and small
// enough that a regression costs the test run 64 MiB rather than 2 GiB.
const declaredCredLen = 64 << 20

// allocBudget bounds what decoding a 32-byte message may allocate. Decoding one
// well-formed call header allocates a few hundred bytes, so the budget leaves
// three orders of magnitude of headroom while still failing hard if the decoder
// sizes a buffer from the declared length instead of the delivered bytes.
const allocBudget = 8 << 20

// credOverrunCall builds a complete 32-byte RPC call header whose credential
// body claims declaredCredLen bytes that were never sent: six header words, the
// credential flavor, and the credential length. Nothing follows, so a decoder
// that trusts the length reads past the end of everything the peer delivered.
func credOverrunCall() []byte {
	msg := make([]byte, 32)
	put := func(i int, v uint32) { binary.BigEndian.PutUint32(msg[i*4:], v) }
	put(0, 1)       // XID
	put(1, RPCCall) // MsgType
	put(2, 2)       // RPCVersion
	put(3, 100003)  // Program: NFS
	put(4, 3)       // Version
	put(5, 0)       // Procedure: NULL
	put(6, 1)       // Cred.Flavor: AUTH_UNIX
	put(7, declaredCredLen)
	return msg
}

// allocatedBy reports the bytes fn allocates, so a test can assert on the size
// of a buffer the code under test never returns.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestReadCall_CredLengthDoesNotSizeAllocation pins the rule that a declared
// length never sizes an allocation on its own: the peer's 32 bytes are the
// ceiling, not the 64 MiB the message asks for. ReadCall runs before any
// credential is inspected, on every transport, so a message that allocates from
// its own length field is an unauthenticated peer's lever on server memory.
func TestReadCall_CredLengthDoesNotSizeAllocation(t *testing.T) {
	msg := credOverrunCall()

	var err error
	allocated := allocatedBy(func() { _, err = ReadCall(msg) })

	if err == nil {
		t.Fatal("ReadCall accepted a call whose credential body was never sent")
	}
	if allocated > allocBudget {
		t.Errorf("ReadCall allocated %d bytes decoding a %d-byte message (budget %d): "+
			"the declared credential length sized the buffer, not the delivered bytes",
			allocated, len(msg), allocBudget)
	}
}

// TestReadCall_WellFormedCallStillDecodes guards the other direction: capping
// reads at the delivered length must not reject a legitimate call, whose
// credential body is by definition present in the bytes the peer sent.
func TestReadCall_WellFormedCallStillDecodes(t *testing.T) {
	body := []byte{0, 0, 0, 42} // a four-byte AUTH_UNIX credential body
	msg := make([]byte, 0, 40)
	word := func(v uint32) { msg = binary.BigEndian.AppendUint32(msg, v) }
	word(7)       // XID
	word(RPCCall) // MsgType
	word(2)       // RPCVersion
	word(100003)  // Program
	word(3)       // Version
	word(0)       // Procedure
	word(1)       // Cred.Flavor
	word(uint32(len(body)))
	msg = append(msg, body...)
	word(0) // Verf.Flavor: AUTH_NULL
	word(0) // Verf.Body: empty

	call, err := ReadCall(msg)
	if err != nil {
		t.Fatalf("ReadCall rejected a well-formed call: %v", err)
	}
	if call.XID != 7 {
		t.Errorf("XID = %d, want 7", call.XID)
	}
	if got := string(call.Cred.Body); got != string(body) {
		t.Errorf("Cred.Body = %q, want %q", got, body)
	}
}
