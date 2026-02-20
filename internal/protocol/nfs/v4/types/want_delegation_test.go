package types

import (
	"bytes"
	"testing"
)

func TestWantDelegationArgs_RoundTrip_ClaimPrevious(t *testing.T) {
	original := WantDelegationArgs{
		Want: OPEN4_SHARE_ACCESS_READ,
		Claim: WantDelegationClaim{
			ClaimType: CLAIM_PREVIOUS,
			DelegType: OPEN_DELEGATE_READ,
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded WantDelegationArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Want != original.Want {
		t.Errorf("Want: got 0x%x, want 0x%x", decoded.Want, original.Want)
	}
	if decoded.Claim.ClaimType != CLAIM_PREVIOUS {
		t.Errorf("ClaimType: got %d, want %d", decoded.Claim.ClaimType, CLAIM_PREVIOUS)
	}
	if decoded.Claim.DelegType != OPEN_DELEGATE_READ {
		t.Errorf("DelegType: got %d, want %d", decoded.Claim.DelegType, OPEN_DELEGATE_READ)
	}
}

func TestWantDelegationArgs_RoundTrip_ClaimNull(t *testing.T) {
	original := WantDelegationArgs{
		Want: OPEN4_SHARE_ACCESS_BOTH,
		Claim: WantDelegationClaim{
			ClaimType: CLAIM_NULL,
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded WantDelegationArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Want != original.Want {
		t.Errorf("Want: got 0x%x, want 0x%x", decoded.Want, original.Want)
	}
	if decoded.Claim.ClaimType != CLAIM_NULL {
		t.Errorf("ClaimType: got %d, want %d", decoded.Claim.ClaimType, CLAIM_NULL)
	}
}

func TestWantDelegationRes_RoundTrip_OK_None(t *testing.T) {
	original := WantDelegationRes{
		Status:         NFS4_OK,
		DelegationType: OPEN_DELEGATE_NONE,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded WantDelegationRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.DelegationType != OPEN_DELEGATE_NONE {
		t.Errorf("DelegationType: got %d, want %d", decoded.DelegationType, OPEN_DELEGATE_NONE)
	}
}

func TestWantDelegationRes_RoundTrip_Error(t *testing.T) {
	original := WantDelegationRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded WantDelegationRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestWantDelegationArgs_String(t *testing.T) {
	args := WantDelegationArgs{Want: 1, Claim: WantDelegationClaim{ClaimType: CLAIM_PREVIOUS, DelegType: OPEN_DELEGATE_WRITE}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
