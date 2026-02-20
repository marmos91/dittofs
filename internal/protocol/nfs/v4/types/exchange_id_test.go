package types

import (
	"bytes"
	"testing"
)

// TestExchangeIdArgs_RoundTrip tests encode/decode with SP4_NONE and 1 impl_id.
func TestExchangeIdArgs_RoundTrip(t *testing.T) {
	original := &ExchangeIdArgs{
		ClientOwner:  ValidClientOwner(),
		Flags:        EXCHGID4_FLAG_USE_NON_PNFS | EXCHGID4_FLAG_SUPP_MOVED_REFER,
		StateProtect: ValidStateProtectNone(),
		ClientImplId: []NfsImplId4{ValidNfsImplId()},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &ExchangeIdArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	// Verify fields
	if decoded.ClientOwner.Verifier != original.ClientOwner.Verifier {
		t.Errorf("Verifier = %x, want %x", decoded.ClientOwner.Verifier, original.ClientOwner.Verifier)
	}
	if !bytes.Equal(decoded.ClientOwner.OwnerID, original.ClientOwner.OwnerID) {
		t.Errorf("OwnerID = %x, want %x", decoded.ClientOwner.OwnerID, original.ClientOwner.OwnerID)
	}
	if decoded.Flags != original.Flags {
		t.Errorf("Flags = 0x%x, want 0x%x", decoded.Flags, original.Flags)
	}
	if decoded.StateProtect.How != SP4_NONE {
		t.Errorf("StateProtect.How = %d, want %d", decoded.StateProtect.How, SP4_NONE)
	}
	if len(decoded.ClientImplId) != 1 {
		t.Fatalf("ClientImplId length = %d, want 1", len(decoded.ClientImplId))
	}
	if decoded.ClientImplId[0].Domain != original.ClientImplId[0].Domain {
		t.Errorf("ImplId.Domain = %q, want %q", decoded.ClientImplId[0].Domain, original.ClientImplId[0].Domain)
	}
	if decoded.ClientImplId[0].Name != original.ClientImplId[0].Name {
		t.Errorf("ImplId.Name = %q, want %q", decoded.ClientImplId[0].Name, original.ClientImplId[0].Name)
	}
	if decoded.ClientImplId[0].Date.Seconds != original.ClientImplId[0].Date.Seconds {
		t.Errorf("ImplId.Date.Seconds = %d, want %d", decoded.ClientImplId[0].Date.Seconds, original.ClientImplId[0].Date.Seconds)
	}

	// Verify String() doesn't panic
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestExchangeIdArgs_RoundTrip_NoImplId tests with 0 impl_ids (empty array).
func TestExchangeIdArgs_RoundTrip_NoImplId(t *testing.T) {
	original := &ExchangeIdArgs{
		ClientOwner:  ValidClientOwner(),
		Flags:        EXCHGID4_FLAG_USE_NON_PNFS,
		StateProtect: ValidStateProtectNone(),
		ClientImplId: nil, // no impl id
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &ExchangeIdArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded.ClientImplId) != 0 {
		t.Errorf("ClientImplId length = %d, want 0", len(decoded.ClientImplId))
	}
}

// TestExchangeIdArgs_RoundTrip_MachCred tests with SP4_MACH_CRED state protection.
func TestExchangeIdArgs_RoundTrip_MachCred(t *testing.T) {
	original := &ExchangeIdArgs{
		ClientOwner: ValidClientOwner(),
		Flags:       EXCHGID4_FLAG_USE_NON_PNFS,
		StateProtect: StateProtect4A{
			How:        SP4_MACH_CRED,
			MachOps:    Bitmap4{0x00000003, 0x00000001},
			EnforceOps: Bitmap4{0x00000002},
		},
		ClientImplId: []NfsImplId4{ValidNfsImplId()},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &ExchangeIdArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.StateProtect.How != SP4_MACH_CRED {
		t.Errorf("StateProtect.How = %d, want %d", decoded.StateProtect.How, SP4_MACH_CRED)
	}
	if len(decoded.StateProtect.MachOps) != 2 {
		t.Fatalf("MachOps length = %d, want 2", len(decoded.StateProtect.MachOps))
	}
	if decoded.StateProtect.MachOps[0] != 0x00000003 {
		t.Errorf("MachOps[0] = 0x%x, want 0x00000003", decoded.StateProtect.MachOps[0])
	}
	if decoded.StateProtect.MachOps[1] != 0x00000001 {
		t.Errorf("MachOps[1] = 0x%x, want 0x00000001", decoded.StateProtect.MachOps[1])
	}
	if len(decoded.StateProtect.EnforceOps) != 1 {
		t.Fatalf("EnforceOps length = %d, want 1", len(decoded.StateProtect.EnforceOps))
	}
	if decoded.StateProtect.EnforceOps[0] != 0x00000002 {
		t.Errorf("EnforceOps[0] = 0x%x, want 0x00000002", decoded.StateProtect.EnforceOps[0])
	}
}

// TestExchangeIdRes_RoundTrip tests a success response with all fields.
func TestExchangeIdRes_RoundTrip(t *testing.T) {
	original := &ExchangeIdRes{
		Status:       NFS4_OK,
		ClientID:     0x123456789abcdef0,
		SequenceID:   1,
		Flags:        EXCHGID4_FLAG_USE_NON_PNFS | EXCHGID4_FLAG_CONFIRMED_R,
		StateProtect: StateProtect4R{How: SP4_NONE},
		ServerOwner:  ValidServerOwner(),
		ServerScope:  []byte("dittofs-test-scope"),
		ServerImplId: []NfsImplId4{ValidNfsImplId()},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &ExchangeIdRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.ClientID != original.ClientID {
		t.Errorf("ClientID = 0x%x, want 0x%x", decoded.ClientID, original.ClientID)
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.Flags != original.Flags {
		t.Errorf("Flags = 0x%x, want 0x%x", decoded.Flags, original.Flags)
	}
	if decoded.StateProtect.How != SP4_NONE {
		t.Errorf("StateProtect.How = %d, want %d", decoded.StateProtect.How, SP4_NONE)
	}
	if decoded.ServerOwner.MinorID != original.ServerOwner.MinorID {
		t.Errorf("ServerOwner.MinorID = %d, want %d", decoded.ServerOwner.MinorID, original.ServerOwner.MinorID)
	}
	if !bytes.Equal(decoded.ServerOwner.MajorID, original.ServerOwner.MajorID) {
		t.Errorf("ServerOwner.MajorID = %x, want %x", decoded.ServerOwner.MajorID, original.ServerOwner.MajorID)
	}
	if !bytes.Equal(decoded.ServerScope, original.ServerScope) {
		t.Errorf("ServerScope = %x, want %x", decoded.ServerScope, original.ServerScope)
	}
	if len(decoded.ServerImplId) != 1 {
		t.Fatalf("ServerImplId length = %d, want 1", len(decoded.ServerImplId))
	}
	if decoded.ServerImplId[0].Domain != original.ServerImplId[0].Domain {
		t.Errorf("ServerImplId.Domain = %q, want %q", decoded.ServerImplId[0].Domain, original.ServerImplId[0].Domain)
	}

	// Verify String() doesn't panic
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestExchangeIdRes_RoundTrip_Error tests an error response (status-only).
func TestExchangeIdRes_RoundTrip_Error(t *testing.T) {
	original := &ExchangeIdRes{
		Status: NFS4ERR_CLID_INUSE,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &ExchangeIdRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4ERR_CLID_INUSE {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4ERR_CLID_INUSE)
	}
	// No additional fields should be encoded/decoded
	if decoded.ClientID != 0 {
		t.Errorf("ClientID = 0x%x, want 0 (error case)", decoded.ClientID)
	}

	// Verify error String() format
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
