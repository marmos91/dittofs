package attrs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// EncodeACLAttr / DecodeACLAttr Tests
// ============================================================================

func TestACLRoundTrip_MultipleACEs(t *testing.T) {
	original := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Flag:       acl.ACE4_FILE_INHERIT_ACE | acl.ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA,
				Who:        "EVERYONE@",
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       0,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_EXECUTE,
				Who:        "OWNER@",
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       acl.ACE4_INHERITED_ACE,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ACL,
				Who:        "GROUP@",
			},
		},
	}

	var buf bytes.Buffer
	if err := EncodeACLAttr(&buf, original); err != nil {
		t.Fatalf("EncodeACLAttr failed: %v", err)
	}

	decoded, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeACLAttr failed: %v", err)
	}

	if len(decoded.ACEs) != len(original.ACEs) {
		t.Fatalf("decoded ACE count = %d, want %d", len(decoded.ACEs), len(original.ACEs))
	}

	for i, orig := range original.ACEs {
		got := decoded.ACEs[i]
		if got.Type != orig.Type {
			t.Errorf("ACE %d Type = %d, want %d", i, got.Type, orig.Type)
		}
		if got.Flag != orig.Flag {
			t.Errorf("ACE %d Flag = 0x%x, want 0x%x", i, got.Flag, orig.Flag)
		}
		if got.AccessMask != orig.AccessMask {
			t.Errorf("ACE %d AccessMask = 0x%x, want 0x%x", i, got.AccessMask, orig.AccessMask)
		}
		if got.Who != orig.Who {
			t.Errorf("ACE %d Who = %q, want %q", i, got.Who, orig.Who)
		}
	}
}

func TestACLEncode_NilACL(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeACLAttr(&buf, nil); err != nil {
		t.Fatalf("EncodeACLAttr(nil) failed: %v", err)
	}

	// Should be a single uint32 = 0 (zero ACEs)
	if buf.Len() != 4 {
		t.Fatalf("encoded nil ACL length = %d, want 4", buf.Len())
	}

	var aceCount uint32
	if err := binary.Read(bytes.NewReader(buf.Bytes()), binary.BigEndian, &aceCount); err != nil {
		t.Fatalf("read acecount: %v", err)
	}
	if aceCount != 0 {
		t.Errorf("acecount = %d, want 0", aceCount)
	}
}

func TestACLDecode_ZeroACEs(t *testing.T) {
	// Encode 0 ACEs
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(0))

	decoded, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeACLAttr(0 ACEs) failed: %v", err)
	}

	// Should return empty ACL, not nil
	if decoded == nil {
		t.Fatal("decoded ACL is nil, want non-nil with empty ACEs")
	}
	if len(decoded.ACEs) != 0 {
		t.Errorf("decoded ACE count = %d, want 0", len(decoded.ACEs))
	}
}

func TestACLRoundTrip_SpecialIdentifiers(t *testing.T) {
	specials := []string{"OWNER@", "GROUP@", "EVERYONE@"}

	for _, who := range specials {
		t.Run(who, func(t *testing.T) {
			original := &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.ACE4_READ_DATA,
						Who:        who,
					},
				},
			}

			var buf bytes.Buffer
			if err := EncodeACLAttr(&buf, original); err != nil {
				t.Fatalf("EncodeACLAttr failed: %v", err)
			}

			decoded, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("DecodeACLAttr failed: %v", err)
			}

			if len(decoded.ACEs) != 1 {
				t.Fatalf("decoded ACE count = %d, want 1", len(decoded.ACEs))
			}
			if decoded.ACEs[0].Who != who {
				t.Errorf("who = %q, want %q", decoded.ACEs[0].Who, who)
			}
		})
	}
}

func TestACLRoundTrip_UserAtDomain(t *testing.T) {
	original := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       0,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA,
				Who:        "alice@EXAMPLE.COM",
			},
		},
	}

	var buf bytes.Buffer
	if err := EncodeACLAttr(&buf, original); err != nil {
		t.Fatalf("EncodeACLAttr failed: %v", err)
	}

	decoded, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeACLAttr failed: %v", err)
	}

	if decoded.ACEs[0].Who != "alice@EXAMPLE.COM" {
		t.Errorf("who = %q, want %q", decoded.ACEs[0].Who, "alice@EXAMPLE.COM")
	}
}

func TestACLDecode_ExcessiveACECount(t *testing.T) {
	// Encode an ACE count of 200 (exceeds MaxACECount=128)
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(200))

	_, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected error for excessive ACE count, got nil")
	}
}

func TestACLRoundTrip_PreservesAllFields(t *testing.T) {
	original := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Flag:       acl.ACE4_FILE_INHERIT_ACE | acl.ACE4_INHERIT_ONLY_ACE | acl.ACE4_INHERITED_ACE,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_EXECUTE | acl.ACE4_DELETE | acl.ACE4_READ_ACL | acl.ACE4_WRITE_ACL | acl.ACE4_SYNCHRONIZE,
				Who:        "bob@CORP.LOCAL",
			},
			{
				Type:       acl.ACE4_SYSTEM_AUDIT_ACE_TYPE,
				Flag:       acl.ACE4_SUCCESSFUL_ACCESS_ACE_FLAG | acl.ACE4_FAILED_ACCESS_ACE_FLAG,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_DELETE,
				Who:        "EVERYONE@",
			},
			{
				Type:       acl.ACE4_SYSTEM_ALARM_ACE_TYPE,
				Flag:       acl.ACE4_FAILED_ACCESS_ACE_FLAG,
				AccessMask: acl.ACE4_DELETE,
				Who:        "GROUP@",
			},
		},
	}

	var buf bytes.Buffer
	if err := EncodeACLAttr(&buf, original); err != nil {
		t.Fatalf("EncodeACLAttr failed: %v", err)
	}

	decoded, err := DecodeACLAttr(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeACLAttr failed: %v", err)
	}

	if len(decoded.ACEs) != len(original.ACEs) {
		t.Fatalf("ACE count = %d, want %d", len(decoded.ACEs), len(original.ACEs))
	}

	for i := range original.ACEs {
		orig := original.ACEs[i]
		got := decoded.ACEs[i]
		if got.Type != orig.Type || got.Flag != orig.Flag || got.AccessMask != orig.AccessMask || got.Who != orig.Who {
			t.Errorf("ACE %d mismatch:\n  got:  {Type:%d Flag:0x%x Mask:0x%x Who:%q}\n  want: {Type:%d Flag:0x%x Mask:0x%x Who:%q}",
				i, got.Type, got.Flag, got.AccessMask, got.Who,
				orig.Type, orig.Flag, orig.AccessMask, orig.Who)
		}
	}
}

func TestACLEncode_EmptyACL(t *testing.T) {
	emptyACL := &acl.ACL{ACEs: []acl.ACE{}}

	var buf bytes.Buffer
	if err := EncodeACLAttr(&buf, emptyACL); err != nil {
		t.Fatalf("EncodeACLAttr(empty) failed: %v", err)
	}

	// Should encode as 0 ACEs (same as nil)
	var aceCount uint32
	if err := binary.Read(bytes.NewReader(buf.Bytes()), binary.BigEndian, &aceCount); err != nil {
		t.Fatalf("read acecount: %v", err)
	}
	if aceCount != 0 {
		t.Errorf("acecount for empty ACL = %d, want 0", aceCount)
	}
}

// ============================================================================
// EncodeACLSupportAttr Tests
// ============================================================================

func TestEncodeACLSupportAttr(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeACLSupportAttr(&buf); err != nil {
		t.Fatalf("EncodeACLSupportAttr failed: %v", err)
	}

	if buf.Len() != 4 {
		t.Fatalf("encoded length = %d, want 4", buf.Len())
	}

	var value uint32
	if err := binary.Read(bytes.NewReader(buf.Bytes()), binary.BigEndian, &value); err != nil {
		t.Fatalf("read value: %v", err)
	}

	// All four support bits: 0x01 | 0x02 | 0x04 | 0x08 = 0x0F
	expected := uint32(0x0F)
	if value != expected {
		t.Errorf("ACLSUPPORT value = 0x%02x, want 0x%02x", value, expected)
	}
}
