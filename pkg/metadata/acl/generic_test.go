package acl

import "testing"

func TestExpandGenericMask(t *testing.T) {
	const (
		genReadExpanded    = uint32(0x00120089) // FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
		genWriteExpanded   = uint32(0x00120116) // FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
		genExecuteExpanded = uint32(0x001200A0) // FILE_EXECUTE | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
		genAllExpanded     = uint32(0x001F01FF) // FILE_ALL_ACCESS
	)

	tests := []struct {
		name string
		in   uint32
		want uint32
	}{
		{"zero", 0, 0},
		{"GENERIC_READ only", 0x80000000, genReadExpanded},
		{"GENERIC_WRITE only", 0x40000000, genWriteExpanded},
		{"GENERIC_EXECUTE only", 0x20000000, genExecuteExpanded},
		{"GENERIC_ALL only", 0x10000000, genAllExpanded},
		{
			"GENERIC_READ | GENERIC_WRITE",
			0x80000000 | 0x40000000,
			genReadExpanded | genWriteExpanded,
		},
		{
			"GENERIC_READ | GENERIC_EXECUTE",
			0x80000000 | 0x20000000,
			genReadExpanded | genExecuteExpanded,
		},
		{
			"all four generic bits",
			0x80000000 | 0x40000000 | 0x20000000 | 0x10000000,
			genReadExpanded | genWriteExpanded | genExecuteExpanded | genAllExpanded,
		},
		{
			"GENERIC_ALL == FILE_ALL_ACCESS",
			0x10000000,
			0x001F01FF,
		},
		{
			"non-generic specific bit (FILE_READ_DATA) passes through",
			0x00000001,
			0x00000001,
		},
		{
			"non-generic specific bits (DELETE | WRITE_DAC) pass through",
			0x00010000 | 0x00040000,
			0x00010000 | 0x00040000,
		},
		{
			"MAXIMUM_ALLOWED is not generic — passes through",
			0x02000000,
			0x02000000,
		},
		{
			"ACCESS_SYSTEM_SECURITY is not generic — passes through",
			0x01000000,
			0x01000000,
		},
		{
			"GENERIC_READ combined with explicit DELETE",
			0x80000000 | 0x00010000,
			genReadExpanded | 0x00010000,
		},
		{
			"GENERIC bits are stripped after expansion",
			0x80000000,
			genReadExpanded, // GENERIC_READ bit must NOT remain
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandGenericMask(tc.in)
			if got != tc.want {
				t.Errorf("ExpandGenericMask(0x%08x) = 0x%08x; want 0x%08x",
					tc.in, got, tc.want)
			}
			// Post-condition: no generic bits remain.
			const genericBits = uint32(0xF0000000)
			if got&genericBits != 0 {
				t.Errorf("ExpandGenericMask(0x%08x) leaked generic bits: 0x%08x",
					tc.in, got&genericBits)
			}
		})
	}
}

// TestExpandGenericMask_Idempotent verifies that expanding an already-
// expanded mask is a no-op. Once generic bits are stripped, re-application
// must produce the same value (callers that defensively re-expand at
// multiple layers must not see drift).
func TestExpandGenericMask_Idempotent(t *testing.T) {
	inputs := []uint32{
		0x80000000,
		0x40000000,
		0x20000000,
		0x10000000,
		0x80000000 | 0x40000000 | 0x20000000 | 0x10000000,
		0x00000001 | 0x00010000,
	}
	for _, in := range inputs {
		first := ExpandGenericMask(in)
		second := ExpandGenericMask(first)
		if first != second {
			t.Errorf("ExpandGenericMask not idempotent for 0x%08x: first=0x%08x second=0x%08x",
				in, first, second)
		}
	}
}

// TestGenericDerivedBits verifies the best-effort set used to relax the
// MAXIMUM_ALLOWED strict explicit-bit gate: only bits introduced by GENERIC_*
// expansion are reported, never bits the caller named as specific rights.
func TestGenericDerivedBits(t *testing.T) {
	const (
		genReadExpanded    = uint32(0x00120089)
		genExecuteExpanded = uint32(0x001200A0)
	)
	tests := []struct {
		name string
		in   uint32
		want uint32
	}{
		{"no generic, specific only", 0x00000001, 0},
		{"GENERIC_READ only -> full read set", 0x80000000, genReadExpanded},
		{"GENERIC_EXECUTE only -> full execute set", 0x20000000, genExecuteExpanded},
		// GENERIC_WRITE and GENERIC_ALL are NOT best-effort (not in Samba's
		// ok_mask): their mapped bits stay under the strict gate.
		{"GENERIC_WRITE only -> nothing derived", 0x40000000, 0},
		{"GENERIC_ALL only -> nothing derived", 0x10000000, 0},
		// A directly-named specific bit combined with a generic bit: the named
		// bit is NOT generic-derived even though expansion also yields it.
		{"GENERIC_READ | named READ_DATA -> READ_DATA excluded", 0x80000000 | 0x00000001, genReadExpanded &^ 0x00000001},
		{"named WRITE_DATA only -> nothing derived", 0x00000002, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GenericDerivedBits(tt.in); got != tt.want {
				t.Errorf("GenericDerivedBits(0x%08x) = 0x%08x, want 0x%08x", tt.in, got, tt.want)
			}
		})
	}
}
