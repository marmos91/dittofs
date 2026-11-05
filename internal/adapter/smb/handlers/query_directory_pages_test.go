package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/acl"
)

type queryScriptedPageStore struct {
	metadata.Store
	cursors []string
	failAt  int
	calls   int
}

func (s *queryScriptedPageStore) ListChildren(ctx context.Context, handle metadata.FileHandle, _ string, _ int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	s.calls++
	if s.calls == s.failAt {
		return nil, "", fmt.Errorf("metadata page read failed")
	}
	if s.calls > len(s.cursors) {
		return nil, "", fmt.Errorf("unexpected extra metadata page")
	}
	return s.Store.ListChildren(ctx, handle, s.cursors[s.calls-1], 1, attrs)
}

func queryPageNames(t *testing.T, h *Handler, ctx *SMBHandlerContext, id [16]byte, pattern string, flags uint8, size uint32) ([]string, types.Status) {
	t.Helper()
	resp, err := h.QueryDirectory(ctx, &QueryDirectoryRequest{
		FileInfoClass: uint8(types.FileNamesInformation), FileID: id,
		FileName: pattern, Flags: flags, OutputBufferLength: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for data := resp.Data; len(data) > 0; {
		if len(data) < 12 {
			t.Fatal("truncated directory entry")
		}
		next := binary.LittleEndian.Uint32(data)
		nameLen := binary.LittleEndian.Uint32(data[8:])
		if uint64(nameLen)+12 > uint64(len(data)) {
			t.Fatal("truncated directory name")
		}
		names = append(names, decodeUTF16LE(data[12:12+nameLen]))
		if next == 0 {
			break
		}
		if next > uint32(len(data)) || next < 12 {
			t.Fatal("invalid next-entry offset")
		}
		data = data[next:]
	}
	return names, resp.Status
}

func drainPageNames(t *testing.T, h *Handler, ctx *SMBHandlerContext, id [16]byte, pattern string, flags uint8, size uint32, maxCalls int) []string {
	t.Helper()
	var names []string
	for range maxCalls {
		page, status := queryPageNames(t, h, ctx, id, pattern, flags, size)
		if status == types.StatusNoMoreFiles {
			return names
		}
		if status != types.StatusSuccess || len(page) == 0 {
			t.Fatalf("directory query status=%x entries=%d", uint32(status), len(page))
		}
		names = append(names, page...)
		flags &^= uint8(types.SMB2RestartScans)
	}
	t.Fatal("directory enumeration failed to terminate")
	return nil
}

func TestQueryDirectoryMetadataPageBoundary(t *testing.T) {
	for _, n := range []int{5242, 5243} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			names := make([]string, n)
			for i := range names {
				names[i] = fmt.Sprintf("f%05d", i)
			}
			h, open, _, ctx := setupQueryDirTest(t, names)
			got := drainPageNames(t, h, ctx, open.FileID, "*", uint8(types.SMB2RestartScans), 65536, 20)
			want := append([]string{".", ".."}, names...)
			if !slices.Equal(got, want) {
				t.Errorf("enumerated %d entries, want all %d in order", len(got), len(want))
			}
			last, status := queryPageNames(t, h, ctx, open.FileID, names[n-1], uint8(types.SMB2RestartScans), 128)
			if status != types.StatusSuccess || !slices.Equal(last, names[n-1:]) {
				t.Errorf("tail-name lookup returned %v status=%x", last, uint32(status))
			}
			// The matching set straddles the metadata page boundary. Both the
			// single-entry flag and the small output budget must retain it all.
			got = drainPageNames(t, h, ctx, open.FileID, "f0524*", uint8(types.SMB2RestartScans|types.SMB2ReturnSingleEntry), 24, 5)
			if !slices.Equal(got, names[5240:]) {
				t.Errorf("single-entry tail scan = %v, want %v", got, names[5240:])
			}
		})
	}
}

func TestQueryDirectoryPagesGlobalOrderAndLiveChanges(t *testing.T) {
	names := make([]string, 5242)
	for i := range names {
		names[i] = fmt.Sprintf("Z%05d", i)
	}
	// Backend ordering puts these on its second page; SMB's folded ordering
	// must place them before every Z entry, even with a small response buffer.
	names = append(names, "a-first", "b-delete")
	h, open, auth, ctx := setupQueryDirTest(t, names)
	var got []string
	for i, want := range []string{".", "..", "a-first"} {
		flags := uint8(types.SMB2ReturnSingleEntry)
		if i == 0 {
			flags |= uint8(types.SMB2RestartScans)
		}
		page, status := queryPageNames(t, h, ctx, open.FileID, "*", flags, 32)
		if status != types.StatusSuccess || !slices.Equal(page, []string{want}) {
			t.Fatalf("entry %d = %v status=%x, want %s", i, page, uint32(status), want)
		}
		got = append(got, page...)
	}
	metaSvc := h.Registry.GetMetadataService()
	if _, _, err := metaSvc.RemoveFile(auth, open.MetadataHandle, "b-delete"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := metaSvc.CreateFile(auth, open.MetadataHandle, "c-created", &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	got = append(got, drainPageNames(t, h, ctx, open.FileID, "*", 0, 65536, 20)...)
	want := append([]string{".", "..", "a-first", "c-created"}, names[:5242]...)
	if !slices.Equal(got, want) {
		t.Errorf("live enumeration lost ordering or changes: entries=%d want=%d", len(got), len(want))
	}
}

func TestQueryDirectoryPagesAccessBasedEnumeration(t *testing.T) {
	children := make([]abeChild, 5242)
	for i := range children {
		children[i] = abeChild{name: fmt.Sprintf("a%05d", i), uid: 1000, gid: 1000, mode: 0o600, acl: readOnlyACL(acl.SpecialOwner)}
	}
	children = append(children,
		abeChild{name: "z-hidden", uid: 1000, gid: 1000, mode: 0o600, acl: readOnlyACL(acl.SpecialOwner)},
		abeChild{name: "z-visible", uid: 1000, gid: 1000, mode: 0o644, acl: readOnlyACL(acl.SpecialEveryone)},
	)
	h, open, ctx := setupABEQueryDirTest(t, true, 2000, 2000, children)
	got := drainPageNames(t, h, ctx, open.FileID, "*", uint8(types.SMB2RestartScans), 128, 4)
	if want := []string{".", "..", "z-visible"}; !slices.Equal(got, want) {
		t.Errorf("ABE listing = %s, want %v", strings.Join(got, ","), want)
	}
}

func TestQueryDirectoryRejectsIncompletePageScan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cursors []string
		failAt  int
	}{
		// An expired metadata cookie can restart a scan at its first page.
		// Without detection this sequence would successfully emit a,b,a,c.
		{"revisited_page", []string{"", "a", "", "b"}, 0},
		{"later_page_error", []string{"", "a"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, open, _, ctx := setupQueryDirTest(t, []string{"a", "b", "c"})
			metaSvc := h.Registry.GetMetadataService()
			store, err := metaSvc.GetStoreForShare("/test")
			if err != nil {
				t.Fatal(err)
			}
			pages := &queryScriptedPageStore{Store: store, cursors: tc.cursors, failAt: tc.failAt}
			if err := metaSvc.RegisterStoreForShare("/test", pages); err != nil {
				t.Fatal(err)
			}
			names, status := queryPageNames(t, h, ctx, open.FileID, "*", uint8(types.SMB2RestartScans), 65536)
			if status == types.StatusSuccess || len(names) != 0 {
				t.Errorf("incomplete scan returned successful/partial listing: status=%x names=%v", uint32(status), names)
			}
			if open.EnumerationComplete {
				t.Error("failed scan marked enumeration complete")
			}
		})
	}
}
