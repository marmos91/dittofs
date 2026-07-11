package metadata

import (
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// CloneBlocks returns a deep copy of a []block.ChunkRef. ChunkRef is a value
// type (Hash is a [32]byte array, Offset and Size are scalars), so a flat
// element-wise copy fully detaches the clone. Returns nil for a nil/empty
// input so a round-trip preserves the omitempty wire form.
func CloneBlocks(in []block.ChunkRef) []block.ChunkRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]block.ChunkRef, len(in))
	copy(out, in)
	return out
}

// CloneACL returns a deep copy of a *acl.ACL, including fresh copies of its ACEs
// (DACL) and SACL slices. acl.ACE is a flat value type (uint32 fields + a string
// Who), so an element-wise slice copy fully detaches the clone. Returns nil for
// a nil input so a round-trip preserves the "no ACL set" semantics.
func CloneACL(in *acl.ACL) *acl.ACL {
	if in == nil {
		return nil
	}
	out := *in
	if in.ACEs != nil {
		out.ACEs = make([]acl.ACE, len(in.ACEs))
		copy(out.ACEs, in.ACEs)
	}
	if in.SACL != nil {
		out.SACL = make([]acl.ACE, len(in.SACL))
		copy(out.SACL, in.SACL)
	}
	return &out
}

// CloneEAs returns a deep copy of a FileAttr.EAs map, copying each value slice
// so neither side can mutate the other's EA bytes in place. Returns nil for a
// nil/empty input so a round-trip preserves the omitempty wire form.
func CloneEAs(in map[string][]byte) map[string][]byte {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		vc := make([]byte, len(v))
		copy(vc, v)
		out[k] = vc
	}
	return out
}
