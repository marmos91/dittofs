package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// encodeOneEAEntry builds a single FILE_FULL_EA_INFORMATION entry (last in a
// chain, NextEntryOffset = 0) for decode tests.
func encodeOneEAEntry(name string, value []byte) []byte {
	nameBytes := []byte(name)
	entry := make([]byte, 8+len(nameBytes)+1+len(value))
	entry[5] = byte(len(nameBytes))
	entry[6] = byte(len(value))
	entry[7] = byte(len(value) >> 8)
	copy(entry[8:], nameBytes)
	copy(entry[8+len(nameBytes)+1:], value)
	return entry
}

func TestDecodeFullEaEntries_SingleAndMulti(t *testing.T) {
	// Two chained entries: build via the encoder and decode them back.
	encoded := encodeFullEaInformation(map[string][]byte{
		"EAONE":    []byte("first"),
		"SECONDEA": []byte("second"),
	})
	entries, err := decodeFullEaEntries(encoded)
	if err != nil {
		t.Fatalf("decodeFullEaEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("decoded %d entries, want 2", len(entries))
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.name] = string(e.value)
	}
	if got["EAONE"] != "first" || got["SECONDEA"] != "second" {
		t.Fatalf("round-trip mismatch: %v", got)
	}
}

func TestDecodeFullEaEntries_ZeroLengthValue(t *testing.T) {
	buf := encodeOneEAEntry("ZeroEA", nil)
	entries, err := decodeFullEaEntries(buf)
	if err != nil {
		t.Fatalf("decodeFullEaEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].name != "ZeroEA" || len(entries[0].value) != 0 {
		t.Fatalf("unexpected decode: %+v", entries)
	}
}

func TestDecodeFullEaEntries_Truncated(t *testing.T) {
	buf := encodeOneEAEntry("NAME", []byte("value"))
	// Chop the value bytes.
	if _, err := decodeFullEaEntries(buf[:len(buf)-2]); err == nil {
		t.Fatal("expected error on truncated value")
	}
}

func TestEAMutationsFromEntries_DeleteOnEmpty(t *testing.T) {
	muts := eaMutationsFromEntries([]eaEntry{
		{name: "Keep", value: []byte("v")},
		{name: "Drop", value: nil},
	})
	if len(muts) != 2 {
		t.Fatalf("got %d mutations, want 2", len(muts))
	}
	byName := map[string]metadata.EAMutation{}
	for _, m := range muts {
		byName[m.Name] = m
	}
	if byName["Keep"].Delete {
		t.Error("non-empty value must be an upsert, not a delete")
	}
	if !byName["Drop"].Delete {
		t.Error("zero-length value must be a delete")
	}
}

func TestEncodeFullEaInformation_Empty(t *testing.T) {
	if got := encodeFullEaInformation(nil); len(got) != 0 {
		t.Fatalf("empty EAs must encode to empty buffer, got %d bytes", len(got))
	}
}

func TestEncodeFullEaInformation_SkipsReservedACLName(t *testing.T) {
	encoded := encodeFullEaInformation(map[string][]byte{
		reservedACLXattrName: []byte("secret"),
		"Visible":            []byte("ok"),
	})
	entries, err := decodeFullEaEntries(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 1 || entries[0].name != "Visible" {
		t.Fatalf("reserved ACL xattr must be omitted from enumeration: %+v", entries)
	}
}

// TestEncodeFullEaInformation_Alignment asserts every NextEntryOffset lands on
// a 4-byte boundary and the chain terminates with NextEntryOffset = 0, so a
// strict parser (Samba ea_pull_list_chained) accepts the buffer.
func TestEncodeFullEaInformation_Alignment(t *testing.T) {
	encoded := encodeFullEaInformation(map[string][]byte{
		"A":   []byte("x"),     // odd-length entry forces padding
		"BB":  []byte("yy"),    // different length
		"CCC": []byte("zzzzz"), // last entry, unpadded
	})
	offset := 0
	seenLast := false
	for {
		next := int(uint32(encoded[offset]) | uint32(encoded[offset+1])<<8 |
			uint32(encoded[offset+2])<<16 | uint32(encoded[offset+3])<<24)
		if next == 0 {
			seenLast = true
			break
		}
		if next%4 != 0 {
			t.Fatalf("NextEntryOffset %d at %d is not 4-byte aligned", next, offset)
		}
		offset += next
	}
	if !seenLast {
		t.Fatal("chain never terminated with NextEntryOffset = 0")
	}
}

// TestEncodeFullEaInformation_SkipsOversized asserts an EA whose value exceeds
// the uint16 wire field is dropped (not emitted with a wrapped length), and the
// surviving entries still form a valid chain terminating with NextEntryOffset=0.
func TestEncodeFullEaInformation_SkipsOversized(t *testing.T) {
	encoded := encodeFullEaInformation(map[string][]byte{
		"Big":     make([]byte, 0x10000), // 65536 bytes: > uint16 max
		"Small":   []byte("ok"),
		"AlsoBig": make([]byte, 0x10001),
	})
	entries, err := decodeFullEaEntries(encoded)
	if err != nil {
		t.Fatalf("decode after oversized filter: %v", err)
	}
	if len(entries) != 1 || entries[0].name != "Small" {
		t.Fatalf("oversized EAs must be skipped, leaving a valid chain: %+v", entries)
	}
}

func TestFileAttrEAHelpers_CaseInsensitiveAndPreserveCase(t *testing.T) {
	a := &metadata.FileAttr{}
	a.ApplyEAMutations([]metadata.EAMutation{{Name: "MixedEA", Value: []byte("v1")}})

	if v, ok := a.LookupEA("mixedea"); !ok || !bytes.Equal(v, []byte("v1")) {
		t.Fatalf("case-insensitive lookup failed: %v %v", v, ok)
	}

	// Upsert under a different casing updates in place and keeps original case.
	a.ApplyEAMutations([]metadata.EAMutation{{Name: "MIXEDEA", Value: []byte("v2")}})
	if len(a.EAs) != 1 {
		t.Fatalf("case-different upsert created a duplicate: %v", a.EAs)
	}
	if _, ok := a.EAs["MixedEA"]; !ok {
		t.Fatalf("original casing not preserved: %v", a.EAs)
	}
	if v, _ := a.LookupEA("MixedEA"); !bytes.Equal(v, []byte("v2")) {
		t.Fatalf("value not updated: %q", v)
	}

	// Delete via case-insensitive match leaves the map nil (omitempty form).
	a.ApplyEAMutations([]metadata.EAMutation{{Name: "mixedea", Delete: true}})
	if a.EAs != nil {
		t.Fatalf("deleting the last EA must nil the map, got %v", a.EAs)
	}
}
