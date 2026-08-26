package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// refsOf is manifestRefs without the rows a copy cannot hand on: a zero-hash
// row carries no chunk, so it is not part of the ChunkRef list the adapter
// passes from the source's FileAttr.Blocks.
func refsOf(t *testing.T, ms metadata.Store, payloadID string) []block.ChunkRef {
	t.Helper()
	var refs []block.ChunkRef
	for _, r := range manifestRefs(t, ms, payloadID) {
		if r.Hash.IsZero() {
			continue
		}
		refs = append(refs, r)
	}
	if len(refs) == 0 {
		t.Fatalf("%s carved no placeable rows, so the copy would copy nothing", payloadID)
	}
	return refs
}

// serverSideCopy runs the sequence the adapter's reflink helpers run: the
// engine's manifest-only copy, the destination's wholesale manifest
// replacement, then the discard of the local ranges that replacement made
// stale. Driving all three here is what makes the read-back below about the
// destination the adapter leaves behind rather than about a half-applied copy.
// The adapter binds the first two into one transaction; what they leave behind
// is the same either way, and that is what these tests read.
func serverSideCopy(t *testing.T, ctx context.Context, bs *engine.Store, ms metadata.Store, srcHandle, dstHandle metadata.FileHandle, srcPayloadID, dstPayloadID string) {
	t.Helper()
	serverSideCopyRefs(t, ctx, bs, ms, srcHandle, dstHandle, srcPayloadID, dstPayloadID, refsOf(t, ms, srcPayloadID))
}

// serverSideCopyRefs is serverSideCopy with the copied ChunkRef list supplied,
// for the source whose list is legitimately empty.
func serverSideCopyRefs(t *testing.T, ctx context.Context, bs *engine.Store, ms metadata.Store, srcHandle, dstHandle metadata.FileHandle, srcPayloadID, dstPayloadID string, refs []block.ChunkRef) {
	t.Helper()
	srcFile, err := ms.GetFile(ctx, srcHandle)
	if err != nil {
		t.Fatalf("GetFile(src): %v", err)
	}
	newBlocks, err := bs.CopyPayload(ctx, srcPayloadID, dstPayloadID, refs)
	if err != nil {
		t.Fatalf("CopyPayload: %v", err)
	}
	dstFile, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile(dst): %v", err)
	}
	dstFile.Blocks = newBlocks
	dstFile.Size = srcFile.Size
	if err := ms.SetManifest(ctx, dstFile); err != nil {
		t.Fatalf("SetManifest(dst): %v", err)
	}
	if err := bs.DiscardLocalContent(ctx, dstPayloadID); err != nil {
		t.Fatalf("DiscardLocalContent: %v", err)
	}
}

// TestCopy_DestinationServesTheCopiedContent is the byte-level statement of the
// contract a reflink owes: after the copy, reading the destination gives the
// source's content, not the content the copy replaced.
//
// The destination here already holds its own bytes, warm and local, which is
// the case the read path used to answer from and never look further. A
// destination that was empty, or that a client truncated first, reaches the
// manifest through a hole and was always served correctly — which is why an
// `O_TRUNC` opener (`cp --reflink`) sees none of this.
//
// The bystander is the control: it is carved and warm exactly like the
// destination, is not part of the copy, and must still read its own bytes
// afterwards. Without it a discard that dropped every payload's local ranges
// would pass this test on the destination alone.
func TestCopy_DestinationServesTheCopiedContent(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, _ := openOfflineEngine(t, t.TempDir(), ms, mem)
	t.Cleanup(func() { _ = bs.Close() })

	root := createShare(t, ms, "copy")
	src, srcHandle := createRealFile(t, ms, "copy", "src.bin", root)
	dst, dstHandle := createRealFile(t, ms, "copy", "dst.bin", root)
	bystander, _ := createRealFile(t, ms, "copy", "bystander.bin", root)

	const size = 4 * 1024 * 1024
	content := map[string][]byte{}
	for i, pid := range []string{src, dst, bystander} {
		buf := make([]byte, size)
		rand.New(rand.NewSource(int64(i) + 1)).Read(buf) //nolint:gosec // deterministic fixture
		if _, err := bs.WriteAt(ctx, pid, nil, buf, 0); err != nil {
			t.Fatalf("WriteAt(%s): %v", pid, err)
		}
		content[pid] = buf
		carve(t, bs, ctx, pid)
		setSize(t, ctx, ms, pid, size)
	}

	serverSideCopy(t, ctx, bs, ms, srcHandle, dstHandle, src, dst)

	// Read at two known offsets rather than the whole file, so a pass cannot
	// come from the destination being uniformly wrong in a convenient way.
	for _, off := range []uint64{0, 2 * 1024 * 1024} {
		got := make([]byte, 64*1024)
		if _, err := bs.ReadAt(ctx, dst, got, off); err != nil {
			t.Fatalf("ReadAt(dst, %d): %v", off, err)
		}
		want := content[src][off : off+uint64(len(got))]
		if bytes.Equal(got, content[dst][off:off+uint64(len(got))]) {
			t.Fatalf("the destination at offset %d served the content the copy replaced", off)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("the destination at offset %d served neither the copied content nor its own", off)
		}
	}

	back := make([]byte, size)
	if _, err := bs.ReadAt(ctx, bystander, back, 0); err != nil {
		t.Fatalf("ReadAt(bystander): %v", err)
	}
	if !bytes.Equal(back, content[bystander]) {
		t.Error("the copy disturbed a payload it had nothing to do with")
	}
}

// TestCopy_DestinationSparseRangeReadsAsZeros is the half of the contract that
// an interval the destination merely lost does not have.
//
// The source is sparse across [1 MiB, 3 MiB): no manifest row covers it, so the
// destination must read it as the zeros the source has there. The destination
// holds its own real bytes over the whole span before the copy, and the copy
// has to leave that range unresolvable by the local tier without making it
// unreadable — a range marked remote-only instead would fail closed, because
// there is nothing on the remote to hydrate it from.
func TestCopy_DestinationSparseRangeReadsAsZeros(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, _ := openOfflineEngine(t, t.TempDir(), ms, mem)
	t.Cleanup(func() { _ = bs.Close() })

	root := createShare(t, ms, "sparse")
	src, srcHandle := createRealFile(t, ms, "sparse", "src.bin", root)
	dst, dstHandle := createRealFile(t, ms, "sparse", "dst.bin", root)

	const mib = 1024 * 1024
	head := make([]byte, mib)
	tail := make([]byte, mib)
	rand.New(rand.NewSource(11)).Read(head) //nolint:gosec // deterministic fixture
	rand.New(rand.NewSource(12)).Read(tail) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, src, nil, head, 0); err != nil {
		t.Fatalf("WriteAt(src head): %v", err)
	}
	if _, err := bs.WriteAt(ctx, src, nil, tail, 3*mib); err != nil {
		t.Fatalf("WriteAt(src tail): %v", err)
	}
	carve(t, bs, ctx, src)
	setSize(t, ctx, ms, src, 4*mib)

	dstData := make([]byte, 4*mib)
	rand.New(rand.NewSource(13)).Read(dstData) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, dst, nil, dstData, 0); err != nil {
		t.Fatalf("WriteAt(dst): %v", err)
	}
	carve(t, bs, ctx, dst)
	setSize(t, ctx, ms, dst, 4*mib)

	serverSideCopy(t, ctx, bs, ms, srcHandle, dstHandle, src, dst)

	for _, tc := range []struct {
		name string
		off  uint64
		want []byte
	}{
		{name: "head", off: 0, want: head[:64*1024]},
		{name: "sparse", off: 2 * mib, want: make([]byte, 64*1024)},
		{name: "tail", off: 3 * mib, want: tail[:64*1024]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]byte, len(tc.want))
			if _, err := bs.ReadAt(ctx, dst, got, tc.off); err != nil {
				t.Fatalf("ReadAt(dst, %d): %v", tc.off, err)
			}
			if bytes.Equal(got, dstData[tc.off:tc.off+uint64(len(got))]) {
				t.Fatalf("the destination at offset %d served the content the copy replaced", tc.off)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("the destination at offset %d did not serve the source's content", tc.off)
			}
		})
	}
}

// setSize records the size a payload's writes gave it, which createRealFile
// leaves at zero and the copy reads off the source.
func setSize(t *testing.T, ctx context.Context, ms metadata.Store, payloadID string, size uint64) {
	t.Helper()
	file, err := ms.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
	if err != nil || file == nil {
		t.Fatalf("GetFileByPayloadID(%s): %v", payloadID, err)
	}
	file.Size = size
	if err := ms.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs(%s): %v", payloadID, err)
	}
}

// TestCopy_SelfCopyKeepsItsOwnRows pins the one case where the destination's
// surviving rows are not stale: a payload copied onto itself replaced nothing.
// The blocks handed in are the caller's snapshot of the payload's projection,
// not the authority on which of its rows are live, so a row missing from them
// is a row the reap would delete out from under a live file.
func TestCopy_SelfCopyKeepsItsOwnRows(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, _ := openOfflineEngine(t, t.TempDir(), ms, mem)
	t.Cleanup(func() { _ = bs.Close() })

	root := createShare(t, ms, "selfcopy")
	self, _ := createRealFile(t, ms, "selfcopy", "self.bin", root)

	const size = 4 * 1024 * 1024
	data := make([]byte, size)
	rand.New(rand.NewSource(21)).Read(data) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, self, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, self)

	rows := refsOf(t, ms, self)
	// Hand in a snapshot that is missing the payload's last row, the shape a
	// stale projection has.
	if _, err := bs.CopyPayload(ctx, self, self, rows[:len(rows)-1]); err != nil {
		t.Fatalf("CopyPayload self-copy: %v", err)
	}

	after := refsOf(t, ms, self)
	if len(after) != len(rows) {
		t.Errorf("the self-copy reaped %d of the payload's own rows", len(rows)-len(after))
	}
}

// TestCopy_SparseSourceLeavesNothingOfTheDestination is the copy that places no
// blocks at all. A source that was only ever sized — a file created and
// truncated up, never written — carves no rows, so the destination inherits an
// empty manifest and its whole content should read as the zeros the source has.
//
// The copy has no rows of its own to overwrite the destination's with, so the
// destination's rows are the only thing a read can resolve through, and they
// hold the content the copy replaced.
func TestCopy_SparseSourceLeavesNothingOfTheDestination(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	bs, _ := openOfflineEngine(t, t.TempDir(), ms, mem)
	t.Cleanup(func() { _ = bs.Close() })

	root := createShare(t, ms, "sparsesrc")
	src, srcHandle := createRealFile(t, ms, "sparsesrc", "src.bin", root)
	dst, dstHandle := createRealFile(t, ms, "sparsesrc", "dst.bin", root)

	const size = 4 * 1024 * 1024
	setSize(t, ctx, ms, src, size) // sized, never written: no rows to copy

	dstData := make([]byte, size)
	rand.New(rand.NewSource(31)).Read(dstData) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, dst, nil, dstData, 0); err != nil {
		t.Fatalf("WriteAt(dst): %v", err)
	}
	carve(t, bs, ctx, dst)
	setSize(t, ctx, ms, dst, size)

	if rows := refsOfSparse(t, ms, src); len(rows) != 0 {
		t.Fatalf("the source carved %d rows, so it is not the sparse case", len(rows))
	}
	serverSideCopyRefs(t, ctx, bs, ms, srcHandle, dstHandle, src, dst, nil)

	for _, off := range []uint64{0, 2 * 1024 * 1024} {
		got := make([]byte, 64*1024)
		if _, err := bs.ReadAt(ctx, dst, got, off); err != nil {
			t.Fatalf("ReadAt(dst, %d): %v", off, err)
		}
		if bytes.Equal(got, dstData[off:off+uint64(len(got))]) {
			t.Fatalf("the destination at offset %d served the content the copy replaced", off)
		}
		if !bytes.Equal(got, make([]byte, len(got))) {
			t.Fatalf("the destination at offset %d did not read as the source's zeros", off)
		}
	}
}

// refsOfSparse is refsOf without its "carved something" assertion, for the
// source that is supposed to have carved nothing.
func refsOfSparse(t *testing.T, ms metadata.Store, payloadID string) []block.ChunkRef {
	t.Helper()
	var refs []block.ChunkRef
	for _, r := range manifestRefs(t, ms, payloadID) {
		if !r.Hash.IsZero() {
			refs = append(refs, r)
		}
	}
	return refs
}
