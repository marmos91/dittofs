package badger

import (
	"errors"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestGetFileChunkAtOffsetDescendingCandidates(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rows         []*metadata.FileChunk
		remove       string
		corrupt      string
		off          uint64
		want         string
		wantErr      bool
		inconsistent bool
	}{
		{
			name: "greatest covering start after a short candidate",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 1000},
				{ID: "p/100", DataSize: 800},
				{ID: "p/500", DataSize: 4},
				{ID: "p/1000", DataSize: 100},
			},
			off: 750, want: "p/100",
		},
		{
			name: "hole after all candidates",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 10},
				{ID: "p/20", DataSize: 10},
				{ID: "p/100", DataSize: 10},
				{ID: "p/1000", DataSize: 10},
			},
			off: 900,
		},
		{
			name: "stale index before covering straddler",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 1000},
				{ID: "p/100", DataSize: 800},
				{ID: "p/500", DataSize: 4},
			},
			remove: "p/100", off: 750, want: "p/0",
		},
		{
			name: "malformed row does not hide covering straddler",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 1000},
				{ID: "p/500", DataSize: 4},
				{ID: "p/not-an-offset", DataSize: 100},
			},
			off: 750, want: "p/0",
		},
		{
			name: "malformed row refuses uncovered offset",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 10},
				{ID: "p/500", DataSize: 4},
				{ID: "p/not-an-offset", DataSize: 100},
			},
			off: 750, wantErr: true, inconsistent: true,
		},
		{
			name: "high bit row refuses uncovered offset",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 10},
				{ID: "p/500", DataSize: 4},
				{ID: "p/9223372036854775808", DataSize: 100},
			},
			off: 750, wantErr: true, inconsistent: true,
		},
		{
			name: "undecodable earlier row is not skipped",
			rows: []*metadata.FileChunk{
				{ID: "p/0", DataSize: 1000},
				{ID: "p/100", DataSize: 800},
				{ID: "p/500", DataSize: 4},
			},
			corrupt: "p/100", off: 750, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := newSizeTestStore(t)
			for _, row := range tc.rows {
				if err := s.Put(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.db.Update(func(txn *badgerdb.Txn) error {
				if tc.remove != "" {
					return txn.Delete([]byte(fileChunkPrefix + tc.remove))
				}
				if tc.corrupt != "" {
					return txn.Set([]byte(fileChunkPrefix+tc.corrupt), []byte("invalid JSON"))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			row, err := s.GetFileChunkAtOffset(ctx, "p", tc.off)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetFileChunkAtOffset(%d) = %v, %v", tc.off, row, err)
			}
			if tc.inconsistent && !errors.Is(err, block.ErrManifestInconsistent) {
				t.Fatalf("error = %v, want ErrManifestInconsistent", err)
			}
			got := ""
			if row != nil {
				got = row.ID
			}
			if got != tc.want {
				t.Fatalf("row = %q, want %q", got, tc.want)
			}
		})
	}
}

// An index key is a candidate, not a coverage claim: a stale mapping can name
// a row starting after the requested offset. Coverage comes from that row's ID.
func TestGetFileChunkAtOffsetDescendingCandidatesChecksRowOffset(t *testing.T) {
	ctx := t.Context()
	s := newSizeTestStore(t)
	for _, row := range []*metadata.FileChunk{
		{ID: "p/0", DataSize: 1000},
		{ID: "p/1000", DataSize: 1000},
		{ID: "p/500", DataSize: 4},
	} {
		if err := s.Put(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte(fileChunkFilePrefix+"p:100"), []byte("p/1000"))
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetFileChunkAtOffset(ctx, "p", 750)
	if err != nil || row == nil || row.ID != "p/0" {
		t.Fatalf("GetFileChunkAtOffset(750) = %v, %v; want p/0", row, err)
	}
}
