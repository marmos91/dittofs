package shares

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

func cloneAliasHandle(t *testing.T, handle metadata.FileHandle, spelling string) metadata.FileHandle {
	t.Helper()
	share, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)
	text := id.String()
	if spelling == "upper" {
		text = strings.ToUpper(text)
	} else {
		text = strings.ReplaceAll(text, "-", "")
	}
	return metadata.FileHandle(share + ":" + text)
}

func TestCloneCanonicalHandlesSeeAliasDeferredTail(t *testing.T) {
	for _, spelling := range []string{"upper", "compact"} {
		t.Run(spelling, func(t *testing.T) {
			f := newCloneConcurrencyFixture(t, true)
			ctx := context.Background()
			src, dst := f.file("src"), f.file("dst")
			alias := cloneAliasHandle(t, dst, spelling)
			source := bytes.Repeat([]byte{0x47}, 4096)
			old := bytes.Repeat([]byte{0x68}, 8192)
			require.NoError(t, f.write(ctx, src, "src", source, 0))
			require.NoError(t, f.write(ctx, alias, "dst", old, 0))
			persisted, err := f.ms.GetFile(ctx, dst)
			require.NoError(t, err)
			require.Less(t, persisted.Size, uint64(len(old)), "the tail must still depend on deferred metadata")

			err = common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc)
			var se *metadata.StoreError
			require.True(t, errors.As(err, &se), "a canonical clone must see the tail written with an alias: %v", err)
			require.Equal(t, metadata.ErrNotSupported, se.Code)
			f.read("src", source)
			f.read("dst", old)
			after, err := f.ms.GetFile(ctx, dst)
			require.NoError(t, err)
			require.Equal(t, uint64(len(old)), after.Size)

			f.drain(dst, "dst")
			require.NoError(t, f.bs.DiscardLocalContent(ctx, "dst"))
			require.NoError(t, f.bs.Close())
			f.open()
			f.read("dst", old)
		})
	}
}

func TestCloneInvalidatesAliasWriteCache(t *testing.T) {
	for _, spelling := range []string{"upper", "compact"} {
		t.Run(spelling, func(t *testing.T) {
			f := newCloneConcurrencyFixture(t, true)
			ctx := context.Background()
			src, dst := f.file("src"), f.file("dst")
			alias := cloneAliasHandle(t, dst, spelling)
			source := bytes.Repeat([]byte{0x39}, 8192)
			require.NoError(t, f.write(ctx, src, "src", source, 0))
			require.NoError(t, f.write(ctx, alias, "dst", bytes.Repeat([]byte{0x75}, 4096), 0))
			require.NoError(t, common.CloneWholeFile(ctx, f.bs, f.ms, nil, src, dst, "dst", 0, f.svc))
			intent, err := f.svc.PrepareWrite(f.auth, alias, 1)
			require.NoError(t, err)
			require.Equal(t, uint64(len(source)), intent.PreWriteAttr.Size, "alias writes must fetch cloned attributes instead of the pre-clone cache")
			f.read("dst", source)
		})
	}
}
