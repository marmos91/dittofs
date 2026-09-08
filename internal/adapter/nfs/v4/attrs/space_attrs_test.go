package attrs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestFattr4AttributeNumbers pins every FATTR4 constant to the attribute number
// the protocol assigns it. The numbers come from RFC 7530 Tables 3 and 4
// (Sections 5.6 and 5.7) for attributes 0-55, from RFC 8881 Section 5.7 for
// suppattr_exclcreat, from RFC 7862 for clone_blksize, and from RFC 8276 for
// xattr_support.
//
// Attribute numbers are a wire contract with no local freedom: a constant that
// disagrees with the table makes the server answer a client's request for one
// attribute with the value of another, or withhold an attribute the client did
// ask for. The whole block is asserted rather than the few attributes a change
// happens to touch, because the failure is a silent transcription slip and the
// only way to catch it is to check every number against the table.
func TestFattr4AttributeNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"supported_attrs", FATTR4_SUPPORTED_ATTRS, 0},
		{"type", FATTR4_TYPE, 1},
		{"fh_expire_type", FATTR4_FH_EXPIRE_TYPE, 2},
		{"change", FATTR4_CHANGE, 3},
		{"size", FATTR4_SIZE, 4},
		{"link_support", FATTR4_LINK_SUPPORT, 5},
		{"symlink_support", FATTR4_SYMLINK_SUPPORT, 6},
		{"named_attr", FATTR4_NAMED_ATTR, 7},
		{"fsid", FATTR4_FSID, 8},
		{"unique_handles", FATTR4_UNIQUE_HANDLES, 9},
		{"lease_time", FATTR4_LEASE_TIME, 10},
		{"rdattr_error", FATTR4_RDATTR_ERROR, 11},
		{"acl", FATTR4_ACL, 12},
		{"aclsupport", FATTR4_ACLSUPPORT, 13},
		{"filehandle", FATTR4_FILEHANDLE, 19},
		{"fileid", FATTR4_FILEID, 20},
		{"maxfilesize", FATTR4_MAXFILESIZE, 27},
		{"maxread", FATTR4_MAXREAD, 30},
		{"maxwrite", FATTR4_MAXWRITE, 31},
		{"mode", FATTR4_MODE, 33},
		{"numlinks", FATTR4_NUMLINKS, 35},
		{"owner", FATTR4_OWNER, 36},
		{"owner_group", FATTR4_OWNER_GROUP, 37},
		{"rawdev", FATTR4_RAWDEV, 41},
		{"space_avail", FATTR4_SPACE_AVAIL, 42},
		{"space_free", FATTR4_SPACE_FREE, 43},
		{"space_total", FATTR4_SPACE_TOTAL, 44},
		{"space_used", FATTR4_SPACE_USED, 45},
		{"time_access", FATTR4_TIME_ACCESS, 47},
		{"time_access_set", FATTR4_TIME_ACCESS_SET, 48},
		{"time_metadata", FATTR4_TIME_METADATA, 52},
		{"time_modify", FATTR4_TIME_MODIFY, 53},
		{"time_modify_set", FATTR4_TIME_MODIFY_SET, 54},
		{"mounted_on_fileid", FATTR4_MOUNTED_ON_FILEID, 55},
		{"suppattr_exclcreat", FATTR4_SUPPATTR_EXCLCREAT, 75},
		{"clone_blksize", FATTR4_CLONE_BLKSIZE, 77},
		{"xattr_support", FATTR4_XATTR_SUPPORT, 82},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestSupportedAttrsAdvertisesSpaceToV40Clients asserts the three
// filesystem-space attributes reach a 4.0 client. They sit below
// mounted_on_fileid, the highest attribute 4.0 defines, so the narrowing
// SupportedAttrsFor applies must not touch them: a 4.0 client that is not shown
// them never asks, and statfs over the mount has no space figures to report.
func TestSupportedAttrsAdvertisesSpaceToV40Clients(t *testing.T) {
	for minorVersion := uint32(0); minorVersion <= 2; minorVersion++ {
		bitmap := SupportedAttrsFor(minorVersion)
		for _, bit := range []uint32{FATTR4_SPACE_AVAIL, FATTR4_SPACE_FREE, FATTR4_SPACE_TOTAL} {
			if !IsBitSet(bitmap, bit) {
				t.Errorf("SupportedAttrsFor(%d) does not advertise space attribute %d", minorVersion, bit)
			}
		}
	}
}

// TestSupportedAttrsWithholdsNFSv41ACLAttributes asserts the server never
// advertises attributes 58-61. RFC 8881 assigns those to dacl, sacl,
// change_policy and fs_status, none of which this server implements; a 4.1
// client shown them would receive a uint64 where an nfsacl41 or a chg_policy4
// belongs.
func TestSupportedAttrsWithholdsNFSv41ACLAttributes(t *testing.T) {
	for minorVersion := uint32(0); minorVersion <= 2; minorVersion++ {
		bitmap := SupportedAttrsFor(minorVersion)
		for bit := uint32(58); bit <= 61; bit++ {
			if IsBitSet(bitmap, bit) {
				t.Errorf("SupportedAttrsFor(%d) advertises attribute %d, which this server does not implement", minorVersion, bit)
			}
		}
	}
}

// TestEncodeRealFileSpaceAttrs asserts a GETATTR naming the three space
// attributes answers with the filesystem statistics, in ascending attribute
// order as RFC 7530 Section 3.3.10 requires.
func TestEncodeRealFileSpaceAttrs(t *testing.T) {
	stats := &metadata.FilesystemStatistics{
		TotalBytes:     4 << 30,
		UsedBytes:      1 << 30,
		AvailableBytes: 3 << 30,
	}

	var requested []uint32
	SetBit(&requested, FATTR4_SPACE_AVAIL)
	SetBit(&requested, FATTR4_SPACE_FREE)
	SetBit(&requested, FATTR4_SPACE_TOTAL)

	if !NeedsFilesystemStats(requested) {
		t.Fatal("NeedsFilesystemStats() = false for a request naming the space attributes")
	}

	var buf bytes.Buffer
	if err := EncodeRealFileAttrs(&buf, requested, 0, &metadata.File{}, metadata.FileHandle("/s:id"), stats); err != nil {
		t.Fatalf("EncodeRealFileAttrs: %v", err)
	}

	reader := bytes.NewReader(buf.Bytes())
	responseBitmap, err := DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}
	for _, bit := range []uint32{FATTR4_SPACE_AVAIL, FATTR4_SPACE_FREE, FATTR4_SPACE_TOTAL} {
		if !IsBitSet(responseBitmap, bit) {
			t.Fatalf("response bitmap does not carry space attribute %d", bit)
		}
	}

	var opaqueLen uint32
	if err := binary.Read(reader, binary.BigEndian, &opaqueLen); err != nil {
		t.Fatalf("read attr data length: %v", err)
	}
	if opaqueLen != 24 {
		t.Fatalf("attr data length = %d, want 24 (three uint64s)", opaqueLen)
	}

	for _, want := range []struct {
		name  string
		value uint64
	}{
		{"space_avail", stats.AvailableBytes},
		{"space_free", stats.AvailableBytes},
		{"space_total", stats.TotalBytes},
	} {
		var got uint64
		if err := binary.Read(reader, binary.BigEndian, &got); err != nil {
			t.Fatalf("read %s: %v", want.name, err)
		}
		if got != want.value {
			t.Errorf("%s = %d, want %d", want.name, got, want.value)
		}
	}
}
