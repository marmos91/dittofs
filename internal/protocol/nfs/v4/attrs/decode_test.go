package attrs

import (
	"bytes"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// ParseOwnerString Tests
// ============================================================================

func TestParseOwnerString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint32
		wantErr bool
	}{
		{"numeric@domain", "1000@localdomain", 1000, false},
		{"zero@domain", "0@localdomain", 0, false},
		{"bare numeric", "0", 0, false},
		{"bare numeric 1000", "1000", 1000, false},
		{"root@localdomain", "root@localdomain", 0, false},
		{"nobody@localdomain", "nobody@localdomain", 65534, false},
		{"root@otherdomain", "root@otherdomain", 0, false},
		{"invalid name", "alice@localdomain", 0, true},
		{"empty string", "", 0, true},
		{"just at sign", "@localdomain", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOwnerString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOwnerString(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseOwnerString(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ============================================================================
// ParseGroupString Tests
// ============================================================================

func TestParseGroupString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint32
		wantErr bool
	}{
		{"numeric@domain", "1000@localdomain", 1000, false},
		{"zero@domain", "0@localdomain", 0, false},
		{"bare numeric", "0", 0, false},
		{"bare numeric 2000", "2000", 2000, false},
		{"root@localdomain", "root@localdomain", 0, false},
		{"wheel@localdomain", "wheel@localdomain", 0, false},
		{"nogroup@localdomain", "nogroup@localdomain", 65534, false},
		{"nobody@localdomain", "nobody@localdomain", 65534, false},
		{"invalid name", "editors@localdomain", 0, true},
		{"empty string", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGroupString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseGroupString(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseGroupString(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ============================================================================
// DecodeFattr4ToSetAttrs Tests
// ============================================================================

// buildFattr4 encodes a bitmap + opaque attr_vals into an fattr4 byte stream.
func buildFattr4(t *testing.T, bitmap []uint32, attrData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodeBitmap4(&buf, bitmap); err != nil {
		t.Fatalf("encode bitmap: %v", err)
	}
	if err := xdr.WriteXDROpaque(&buf, attrData); err != nil {
		t.Fatalf("encode attr data: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeFattr4ToSetAttrs_Mode(t *testing.T) {
	// Build attr_vals with mode 0755
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o755)

	// Build bitmap with MODE bit set
	var bitmap []uint32
	SetBit(&bitmap, FATTR4_MODE)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, retBitmap, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.Mode == nil {
		t.Fatal("Mode should be set")
	}
	if *setAttrs.Mode != 0o755 {
		t.Errorf("Mode = 0%o, want 0755", *setAttrs.Mode)
	}
	if !IsBitSet(retBitmap, FATTR4_MODE) {
		t.Error("return bitmap should have MODE bit set")
	}
}

func TestDecodeFattr4ToSetAttrs_Size(t *testing.T) {
	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, 4096)

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_SIZE)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.Size == nil {
		t.Fatal("Size should be set")
	}
	if *setAttrs.Size != 4096 {
		t.Errorf("Size = %d, want 4096", *setAttrs.Size)
	}
}

func TestDecodeFattr4ToSetAttrs_Owner(t *testing.T) {
	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "1000@localdomain")

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_OWNER)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.UID == nil {
		t.Fatal("UID should be set")
	}
	if *setAttrs.UID != 1000 {
		t.Errorf("UID = %d, want 1000", *setAttrs.UID)
	}
}

func TestDecodeFattr4ToSetAttrs_OwnerGroup(t *testing.T) {
	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "2000@localdomain")

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_OWNER_GROUP)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.GID == nil {
		t.Fatal("GID should be set")
	}
	if *setAttrs.GID != 2000 {
		t.Errorf("GID = %d, want 2000", *setAttrs.GID)
	}
}

func TestDecodeFattr4ToSetAttrs_ServerTime(t *testing.T) {
	var attrVals bytes.Buffer
	// atime: SET_TO_SERVER_TIME4
	_ = xdr.WriteUint32(&attrVals, SET_TO_SERVER_TIME4)
	// mtime: SET_TO_SERVER_TIME4
	_ = xdr.WriteUint32(&attrVals, SET_TO_SERVER_TIME4)

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_TIME_ACCESS_SET)
	SetBit(&bitmap, FATTR4_TIME_MODIFY_SET)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if !setAttrs.AtimeNow {
		t.Error("AtimeNow should be true")
	}
	if !setAttrs.MtimeNow {
		t.Error("MtimeNow should be true")
	}
	// SET_TO_SERVER_TIME should also populate Atime/Mtime with server time
	// (matching NFSv3 behavior for compatibility with SetFileAttributes)
	if setAttrs.Atime == nil {
		t.Error("Atime should be set to server time")
	}
	if setAttrs.Mtime == nil {
		t.Error("Mtime should be set to server time")
	}
}

func TestDecodeFattr4ToSetAttrs_ClientTime(t *testing.T) {
	refTime := time.Date(2024, 6, 15, 12, 30, 45, 123456789, time.UTC)

	var attrVals bytes.Buffer
	// atime: SET_TO_CLIENT_TIME4 + nfstime4
	_ = xdr.WriteUint32(&attrVals, SET_TO_CLIENT_TIME4)
	_ = xdr.WriteUint64(&attrVals, uint64(refTime.Unix()))
	_ = xdr.WriteUint32(&attrVals, uint32(refTime.Nanosecond()))
	// mtime: SET_TO_CLIENT_TIME4 + nfstime4
	_ = xdr.WriteUint32(&attrVals, SET_TO_CLIENT_TIME4)
	_ = xdr.WriteUint64(&attrVals, uint64(refTime.Unix()))
	_ = xdr.WriteUint32(&attrVals, uint32(refTime.Nanosecond()))

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_TIME_ACCESS_SET)
	SetBit(&bitmap, FATTR4_TIME_MODIFY_SET)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.AtimeNow {
		t.Error("AtimeNow should be false for client time")
	}
	if setAttrs.MtimeNow {
		t.Error("MtimeNow should be false for client time")
	}

	if setAttrs.Atime == nil {
		t.Fatal("Atime should be set")
	}
	if !setAttrs.Atime.Equal(refTime) {
		t.Errorf("Atime = %v, want %v", setAttrs.Atime, refTime)
	}

	if setAttrs.Mtime == nil {
		t.Fatal("Mtime should be set")
	}
	if !setAttrs.Mtime.Equal(refTime) {
		t.Errorf("Mtime = %v, want %v", setAttrs.Mtime, refTime)
	}
}

func TestDecodeFattr4ToSetAttrs_MultipleAttrs(t *testing.T) {
	var attrVals bytes.Buffer
	// Mode (bit 33) comes after Owner (bit 36) in bit order? No: 33 < 36.
	// Bits in ascending order: MODE(33), OWNER(36), TIME_MODIFY_SET(54)
	_ = xdr.WriteUint32(&attrVals, 0o644)                 // MODE
	_ = xdr.WriteXDRString(&attrVals, "1001@localdomain") // OWNER
	_ = xdr.WriteUint32(&attrVals, SET_TO_CLIENT_TIME4)   // TIME_MODIFY_SET how
	_ = xdr.WriteUint64(&attrVals, uint64(1718451045))    // seconds
	_ = xdr.WriteUint32(&attrVals, 0)                     // nseconds

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_MODE)
	SetBit(&bitmap, FATTR4_OWNER)
	SetBit(&bitmap, FATTR4_TIME_MODIFY_SET)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, retBitmap, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.Mode == nil || *setAttrs.Mode != 0o644 {
		t.Errorf("Mode = %v, want 0644", setAttrs.Mode)
	}
	if setAttrs.UID == nil || *setAttrs.UID != 1001 {
		t.Errorf("UID = %v, want 1001", setAttrs.UID)
	}
	if setAttrs.Mtime == nil {
		t.Fatal("Mtime should be set")
	}

	// Verify return bitmap has all three bits set
	if !IsBitSet(retBitmap, FATTR4_MODE) {
		t.Error("return bitmap should have MODE set")
	}
	if !IsBitSet(retBitmap, FATTR4_OWNER) {
		t.Error("return bitmap should have OWNER set")
	}
	if !IsBitSet(retBitmap, FATTR4_TIME_MODIFY_SET) {
		t.Error("return bitmap should have TIME_MODIFY_SET set")
	}
}

func TestDecodeFattr4ToSetAttrs_UnsupportedAttr(t *testing.T) {
	// FATTR4_TYPE (bit 1) is a read-only attribute
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 1) // NF4REG value

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_TYPE) // bit 1 = read-only

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	_, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for unsupported attribute")
	}

	// Check that it's an NFS4StatusError with ATTRNOTSUPP
	if nfsErr, ok := err.(NFS4StatusError); ok {
		if nfsErr.NFS4Status() != 10032 { // NFS4ERR_ATTRNOTSUPP
			t.Errorf("NFS4Status = %d, want NFS4ERR_ATTRNOTSUPP (10032)", nfsErr.NFS4Status())
		}
	} else {
		t.Errorf("expected NFS4StatusError, got %T: %v", err, err)
	}
}

func TestDecodeFattr4ToSetAttrs_InvalidMode(t *testing.T) {
	// Mode value > 07777
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o10000) // Too large

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_MODE)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	_, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}

	if nfsErr, ok := err.(NFS4StatusError); ok {
		if nfsErr.NFS4Status() != 22 { // NFS4ERR_INVAL
			t.Errorf("NFS4Status = %d, want NFS4ERR_INVAL (22)", nfsErr.NFS4Status())
		}
	} else {
		t.Errorf("expected NFS4StatusError, got %T: %v", err, err)
	}
}

func TestDecodeFattr4ToSetAttrs_BadOwner(t *testing.T) {
	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "unknown_user@localdomain")

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_OWNER)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	_, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for bad owner")
	}

	if nfsErr, ok := err.(NFS4StatusError); ok {
		if nfsErr.NFS4Status() != 10039 { // NFS4ERR_BADOWNER
			t.Errorf("NFS4Status = %d, want NFS4ERR_BADOWNER (10039)", nfsErr.NFS4Status())
		}
	} else {
		t.Errorf("expected NFS4StatusError, got %T: %v", err, err)
	}
}

func TestDecodeFattr4ToSetAttrs_EmptyBitmap(t *testing.T) {
	// Empty bitmap = no attributes to set
	var bitmap []uint32 // empty

	data := buildFattr4(t, bitmap, nil)
	setAttrs, retBitmap, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.Mode != nil || setAttrs.UID != nil || setAttrs.GID != nil || setAttrs.Size != nil {
		t.Error("all fields should be nil for empty bitmap")
	}
	if len(retBitmap) != 0 {
		t.Errorf("return bitmap should be empty, got %v", retBitmap)
	}
}

func TestDecodeFattr4ToSetAttrs_SizeZero(t *testing.T) {
	// Truncate to 0
	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, 0)

	var bitmap []uint32
	SetBit(&bitmap, FATTR4_SIZE)

	data := buildFattr4(t, bitmap, attrVals.Bytes())
	setAttrs, _, err := DecodeFattr4ToSetAttrs(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeFattr4ToSetAttrs: %v", err)
	}

	if setAttrs.Size == nil {
		t.Fatal("Size should be set")
	}
	if *setAttrs.Size != 0 {
		t.Errorf("Size = %d, want 0", *setAttrs.Size)
	}
}

// ============================================================================
// WritableAttrs Tests
// ============================================================================

func TestWritableAttrs(t *testing.T) {
	wa := WritableAttrs()

	// All writable attributes should be set
	expected := []uint32{FATTR4_SIZE, FATTR4_MODE, FATTR4_OWNER, FATTR4_OWNER_GROUP, FATTR4_TIME_ACCESS_SET, FATTR4_TIME_MODIFY_SET}
	for _, bit := range expected {
		if !IsBitSet(wa, bit) {
			t.Errorf("WritableAttrs should have bit %d set", bit)
		}
	}

	// Read-only attributes should NOT be set
	readOnly := []uint32{FATTR4_SUPPORTED_ATTRS, FATTR4_TYPE, FATTR4_FH_EXPIRE_TYPE, FATTR4_CHANGE, FATTR4_FSID, FATTR4_FILEID, FATTR4_TIME_ACCESS, FATTR4_TIME_MODIFY}
	for _, bit := range readOnly {
		if IsBitSet(wa, bit) {
			t.Errorf("WritableAttrs should NOT have bit %d set (read-only)", bit)
		}
	}
}
