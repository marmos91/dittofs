package handlers

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/smbenc"
	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/pkg/block"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/controlplane/runtime"
	"github.com/cubbit/dittofs/pkg/metadata"
)

// COPYCHUNK overwrites the requested range even when the destination extends
// beyond it. A whole-file clone's size restrictions must not reject this copy.
func TestIoctlCopyChunk_LocalOnlyPreservesLongerDestination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctlCode uint32
	}{
		{name: "COPYCHUNK", ctlCode: FsctlSrvCopyChunk},
		{name: "COPYCHUNK_WRITE", ctlCode: FsctlSrvCopyChunkWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ctx, srcOpen, dstOpen := setupCopyChunkFrozenFixture(t)
			installLocalOnlyCopyChunkStore(t, h, ctx)
			const sourceSize = 4096
			source := bytes.Repeat([]byte{0x11}, sourceSize)
			original := bytes.Repeat([]byte{0xa5}, sourceSize*2)
			for _, seed := range []struct {
				open *OpenFile
				data []byte
			}{{srcOpen, source}, {dstOpen, original}} {
				written, err := h.Write(ctx, &WriteRequest{
					FileID: seed.open.FileID, Length: uint32(len(seed.data)), Data: seed.data,
				})
				if err != nil {
					t.Fatalf("Write %s: %v", seed.open.Name().Path, err)
				}
				if written.Status != types.StatusSuccess || written.Count != uint32(len(seed.data)) {
					t.Fatalf("Write %s: status=%v count=%d, want SUCCESS/%d", seed.open.Name().Path, written.Status, written.Count, len(seed.data))
				}
			}

			resume, err := h.Ioctl(ctx, buildIoctlRequestBody(FsctlSrvRequestResumeKey, srcOpen.FileID, nil, 32))
			if err != nil {
				t.Fatalf("REQUEST_RESUME_KEY: %v", err)
			}
			if resume.Status != types.StatusSuccess || len(resume.Data) < 48+resumeKeyLen {
				t.Fatalf("REQUEST_RESUME_KEY: status=%v length=%d, want SUCCESS with resume key", resume.Status, len(resume.Data))
			}

			input := smbenc.NewWriter(32 + 24)
			input.WriteBytes(resume.Data[48 : 48+resumeKeyLen])
			input.WriteUint32(1) // ChunkCount
			input.WriteUint32(0) // Reserved
			input.WriteUint64(0) // SourceOffset
			input.WriteUint64(0) // TargetOffset
			input.WriteUint32(sourceSize)
			input.WriteUint32(0) // Reserved
			copied, err := h.Ioctl(ctx, buildIoctlRequestBody(tc.ctlCode, dstOpen.FileID, input.Bytes(), copyChunkResponseLen))
			if err != nil {
				t.Fatalf("COPYCHUNK: %v", err)
			}
			if copied.Status != types.StatusSuccess || len(copied.Data) != 48+copyChunkResponseLen {
				t.Fatalf("COPYCHUNK: status=%v length=%d, want SUCCESS with copy counts", copied.Status, len(copied.Data))
			}
			counts := smbenc.NewReader(copied.Data[48:])
			chunks, chunkBytes, total := counts.ReadUint32(), counts.ReadUint32(), counts.ReadUint32()
			if chunks != 1 || chunkBytes != 0 || total != sourceSize {
				t.Fatalf("COPYCHUNK counts = (%d, %d, %d), want (1, 0, %d)", chunks, chunkBytes, total, sourceSize)
			}

			file, err := h.Registry.GetMetadataService().GetFile(ctx.Context, dstOpen.MetadataHandle)
			if err != nil {
				t.Fatalf("GetFile destination: %v", err)
			}
			if file.Size != uint64(len(original)) {
				t.Errorf("destination size = %d, want %d", file.Size, len(original))
			}
			read, err := h.Read(ctx, &ReadRequest{FileID: dstOpen.FileID, Length: uint32(len(original))})
			if err != nil {
				t.Fatalf("Read destination: %v", err)
			}
			if read.Status != types.StatusSuccess || len(read.Data) != len(original) {
				t.Fatalf("Read destination: status=%v length=%d, want SUCCESS/%d", read.Status, len(read.Data), len(original))
			}
			if !bytes.Equal(read.Data[:sourceSize], source) {
				t.Error("destination prefix does not match the copied source")
			}
			if !bytes.Equal(read.Data[sourceSize:], original[sourceSize:]) {
				t.Error("destination tail outside the copied range was modified")
			}
			unchanged, err := h.Read(ctx, &ReadRequest{FileID: srcOpen.FileID, Length: sourceSize * 2})
			if err != nil {
				t.Fatalf("Read source after copy: %v", err)
			}
			if unchanged.Status != types.StatusSuccess || !bytes.Equal(unchanged.Data, source) {
				t.Errorf("source changed after copy: status=%v length=%d", unchanged.Status, len(unchanged.Data))
			}
		})
	}
}

// The shared fixture provisions a remote memory store. Replace it with a
// journal-only engine to exercise COPYCHUNK's local read/write path; persistence
// and manifest publication are covered by the block-store tests.
func installLocalOnlyCopyChunkStore(t *testing.T, h *Handler, ctx *SMBHandlerContext) {
	t.Helper()
	rt, ok := h.Registry.(*runtime.Runtime)
	if !ok {
		t.Fatalf("handler registry %T must be a real runtime", h.Registry)
	}
	previous, err := h.Registry.GetBlockStoreForShare(ctx.ShareName)
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Close(); err != nil {
		t.Fatalf("close previous block store: %v", err)
	}
	ms, err := rt.GetMetadataStoreForShare(ctx.ShareName)
	if err != nil {
		t.Fatal(err)
	}
	chunks, ok := ms.(block.EngineFileChunkStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement EngineFileChunkStore", ms)
	}
	hashes, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement SyncedHashStore", ms)
	}
	local, err := journal.Open(t.TempDir(), journal.Config{MaxLocalBytes: 100 * 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	syncer := engine.NewRemoteSync(local, nil, chunks, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local: local, RemoteSync: syncer, FileChunkStore: chunks, SyncedHashStore: hashes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	if err := bs.Start(ctx.Context); err != nil {
		t.Fatal(err)
	}
	if bs.HasRemoteStore() {
		t.Fatal("fixture must exercise a local-only block store")
	}
	if err := rt.SetBlockStoreForTesting(ctx.ShareName, bs); err != nil {
		t.Fatal(err)
	}
}
