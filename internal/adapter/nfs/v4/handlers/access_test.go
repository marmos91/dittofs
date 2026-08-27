package handlers

import (
	"fmt"
	"testing"
)

// allValidAccessBits is the ACCESS4 set RFC 7530 Section 16.1.2 defines. The
// RFC 8276 extended-attribute bits sit above it and belong to minorversion 2.
const allValidAccessBits = uint32(ACCESS4_READ | ACCESS4_LOOKUP | ACCESS4_MODIFY |
	ACCESS4_EXTEND | ACCESS4_DELETE | ACCESS4_EXECUTE)

// TestAccessSupportedFor holds the reply's "supported" field to the three rules
// the RFC states, over every request an NFSv4.0 client can make. It mirrors what
// pynfs asserts in st_access._try_all_combos and _try_invalid, so a change that
// widens the reported set fails here rather than in a suite run.
//
// The narrowing to the requested bits happens at the call site, so the test
// applies the same mask the handlers do.
func TestAccessSupportedFor(t *testing.T) {
	objects := []struct {
		name  string
		isDir bool
		// forbid is the set with no meaning for this object type: RFC 7530
		// Section 16.1.4 for LOOKUP and EXECUTE, Section 16.1.5 for DELETE.
		forbid uint32
	}{
		{"directory", true, ACCESS4_EXECUTE},
		{"non-directory", false, ACCESS4_LOOKUP | ACCESS4_DELETE},
	}

	for _, obj := range objects {
		for _, minor := range []uint32{0, 1, 2} {
			t.Run(fmt.Sprintf("%s/minor%d", obj.name, minor), func(t *testing.T) {
				for requested := uint32(0); requested <= allValidAccessBits; requested++ {
					supported := accessSupportedFor(obj.isDir, minor) & requested

					if extra := supported &^ requested; extra != 0 {
						t.Fatalf("minor %d, requested 0x%02x: supported 0x%02x is not a subset (extra 0x%02x)",
							minor, requested, supported, extra)
					}
					if meaningless := supported & obj.forbid; meaningless != 0 {
						t.Fatalf("minor %d, requested 0x%02x: supported 0x%02x claims bits with no meaning for a %s (0x%02x)",
							minor, requested, supported, obj.name, meaningless)
					}
				}
			})
		}
	}
}

// TestAccessSupportedForUndefinedBits pins the minorversion gate on the RFC 8276
// extended-attribute bits. A v4.0 or v4.1 client that asks for one must not be
// told the server checks it, because the dispatch table refuses the xattr
// operations it would advertise below minorversion 2.
func TestAccessSupportedForUndefinedBits(t *testing.T) {
	// The values pynfs sends in st_access._try_invalid.
	invalid := []uint32{64, 65, 66, 127, 128, 129}

	for _, isDir := range []bool{true, false} {
		for _, minor := range []uint32{0, 1} {
			for _, requested := range invalid {
				supported := accessSupportedFor(isDir, minor) & requested
				if undefined := supported &^ allValidAccessBits; undefined != 0 {
					t.Errorf("isDir=%v minor=%d requested=0x%02x: supported 0x%02x reports undefined bits 0x%02x",
						isDir, minor, requested, supported, undefined)
				}
			}
		}
	}

	// Minorversion 2 defines them, so there the bits are reportable.
	if got := accessSupportedFor(false, 2) & ACCESS4_XAREAD; got != ACCESS4_XAREAD {
		t.Errorf("minor 2 XAREAD = 0x%02x, want 0x%02x", got, ACCESS4_XAREAD)
	}
}
