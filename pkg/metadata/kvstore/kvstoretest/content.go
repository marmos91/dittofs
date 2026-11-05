package kvstoretest

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// A block that carries one chunk twice is refused, since the repeat would
// count the chunk twice against the block's live count, and the refusal
// leaves the block's intent in place for a correct commit.
func testCommitRefusesRepeatedChunk(t *testing.T, kvo Opener) {
	const mib = 1 << 20
	f := newFS(t, kvo, entity.FoldIdentity)
	_, file := f.create(f.admin, f.root, "data", regular(0o644))
	var ns entity.NamespaceID
	for sh, err := range f.s.ControlPlane().Shares(ctx) {
		must(t, err)
		if sh.ID == f.share.ID {
			ns = sh.Namespaces[0].Namespace
		}
	}
	var name entity.BlockName
	name[31] = 1
	var a, b entity.ChunkHash
	a[31], b[31] = 1, 2
	must(t, f.s.Content().Intend(ctx, ns, name, kvstore.Intent{Domain: kvstore.DomainShard}))

	commit := func(second entity.ChunkHash) error {
		v := entity.VersionOf(1)
		_, err := f.s.Content().Commit(ctx, kvstore.BlockCommit{Namespace: ns, Name: name, Size: 2 * mib,
			Chunks: []kvstore.ChunkAt{{Hash: a, Length: mib}, {Hash: second, Position: mib, Length: mib}},
			Files: []kvstore.FileCommit{{Share: f.share.ID, File: file.ID, Refs: []entity.ChunkRef{
				{Length: mib, Chunk: a, Oldest: v, Newest: v},
				{Offset: mib, Length: mib, Chunk: second, Oldest: v, Newest: v}}}}})
		return err
	}
	if err := commit(a); !errors.Is(err, kvstore.ErrInconsistent) {
		t.Fatalf("a block carrying a chunk twice: %v, want ErrInconsistent", err)
	}
	if err := commit(b); err != nil {
		t.Fatalf("the corrected commit after a refusal: %v", err)
	}
}
