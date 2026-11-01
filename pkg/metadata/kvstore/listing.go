package kvstore

import (
	"bytes"
	"context"
	"iter"
	"math"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Listed is one entry of a listing and the cookie that resumes the listing
// after it. File is the child's record, in EntriesPlus only.
type Listed struct {
	Entry  entity.Entry
	Cookie uint64
	File   entity.File
}

// entriesPage is how many entries one read transaction of a listing returns,
// before it finishes the collision chain it is in.
var entriesPage = 256

// Entries lists a directory in digest order, resuming after the cookie
// after; zero starts at the beginning. A cookie is the digest of the entry
// it follows, so it resolves without stored state, on any node, even when
// that entry is gone: an entry present throughout is returned exactly once.
func (n Namespace) Entries(ctx context.Context, id entity.Identity, dir entity.Handle, after uint64) iter.Seq2[Listed, error] {
	return n.list(ctx, id, dir, after, false)
}

// EntriesPlus is Entries with each child's File.
func (n Namespace) EntriesPlus(ctx context.Context, id entity.Identity, dir entity.Handle, after uint64) iter.Seq2[Listed, error] {
	return n.list(ctx, id, dir, after, true)
}

func (n Namespace) list(ctx context.Context, id entity.Identity, dir entity.Handle, after uint64, plus bool) iter.Seq2[Listed, error] {
	return func(yield func(Listed, error) bool) {
		for {
			var page []Listed
			err := n.s.kv.View(ctx, func(r kv.Reader) (err error) {
				page, err = listPage(r, id, dir, after, plus)
				return err
			})
			if err != nil {
				yield(Listed{}, err)
				return
			}
			for _, l := range page {
				if !yield(l, nil) {
					return
				}
			}
			if len(page) < entriesPage {
				return
			}
			after = page[len(page)-1].Cookie
		}
	}
}

// listPage reads one page: entriesPage entries, then the rest of the
// collision chain the last one is in, so the page never ends inside a chain
// and the last digest alone says where to resume. A chain longer than a page
// comes back whole. EntriesPlus reads the page's children in one batch.
func listPage(r kv.Reader, id entity.Identity, dir entity.Handle, after uint64, plus bool) ([]Listed, error) {
	share, d, err := dirIn(r, dir)
	if err != nil {
		return nil, err
	}
	if err := authorize(r, id, d, entity.ListDirectory); err != nil {
		return nil, err
	}
	// Every digest has its top bit clear, so no entry follows the largest
	// cookie, and its successor would wrap to the start of the directory.
	if after == math.MaxUint64 {
		return nil, nil
	}
	prefix := entriesPrefix(share, d.ID)
	var cursor []byte
	if after != 0 {
		cursor = entriesAfter(share, d.ID, after)
	}
	var page []Listed
	for row, err := range r.Scan(prefix, cursor) {
		if err != nil {
			return nil, err
		}
		digest, _, err := keys.ReadUint64(row.Key[len(prefix):])
		if err != nil {
			return nil, err
		}
		if len(page) >= entriesPage && digest != page[len(page)-1].Cookie {
			break
		}
		e, err := decodeEntry(d.ID, bytes.Clone(row.Value))
		if err != nil {
			return nil, err
		}
		page = append(page, Listed{Entry: e, Cookie: digest})
	}
	if plus && len(page) > 0 {
		ks := make([][]byte, len(page))
		for i := range page {
			ks[i] = fileKey(share, page[i].Entry.Child)
		}
		vs, err := r.GetMany(ks)
		if err != nil {
			return nil, err
		}
		for i, v := range vs {
			if v == nil {
				return nil, entity.ErrStale
			}
			if page[i].File, err = decodeFile(share, page[i].Entry.Child, v); err != nil {
				return nil, err
			}
		}
	}
	return page, nil
}
