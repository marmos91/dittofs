package metadata_test

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// The window these tests place a writer in is the gap between the pre-read
// SetFileAttributes takes before any permission check and the transaction it
// later opens to commit. Deciding which columns to write back by diffing the
// working copy against that pre-read makes a request whose value already equals
// the pre-read value indistinguishable from no request at all: the column is
// skipped, the row keeps whatever the peer committed, and the call still
// returns success. Every test here therefore has the caller ask for the value
// it read — asserting that chmod 0644 on an already-0644 file leaves it 0644
// passes with the column never written.

// windowPeer installs peer as the one write that commits inside that gap and
// returns the assertion that it actually ran. A hook that never fires makes
// every assertion in these tests vacuous.
func windowPeer(t *testing.T, ws *windowStore, peer func()) func() {
	t.Helper()
	fired := false
	ws.beforeTx = func() {
		fired = true
		peer()
	}
	return func() {
		t.Helper()
		require.True(t, fired,
			"the window hook never fired: SetFileAttributes opened no transaction through the wrapper")
	}
}

// lostUpdateFixture builds a sqlite-backed Service whose transaction entry is
// hooked, plus one regular file created with mode.
func lostUpdateFixture(t *testing.T, name string, mode uint32) (*metadata.Service, *windowStore, metadata.FileHandle) {
	t.Helper()
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	created, _, err := svc.CreateFile(rootAuth(), rootHandle, name,
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: mode})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)
	return svc, ws, handle
}

func TestSetFileAttributes_ModeEqualToPreReadIsStillWritten(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "mode.bin", 0o644)
	root := rootAuth()

	assertFired := windowPeer(t, ws, func() {
		peer := uint32(0o600)
		_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &peer})
		require.NoError(t, err)
	})

	same := uint32(0o644)
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &same})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint32(0o644), after.Mode&0o7777,
		"chmod 0644 reported success and left mode %#o: the request equalled the value the call "+
			"read, so the column was never written and the peer's chmod survived", after.Mode&0o7777)
}

func TestSetFileAttributes_HiddenEqualToPreReadIsStillWritten(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "hidden.bin", 0o644)
	root := rootAuth()

	assertFired := windowPeer(t, ws, func() {
		peer := true
		_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Hidden: &peer})
		require.NoError(t, err)
	})

	same := false
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Hidden: &same})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.False(t, after.Hidden,
		"clearing Hidden reported success and left it set: the request equalled the value the "+
			"call read, so the column was never written")
}

// The timestamp cases are the ones most likely to be hit in the field: a sync
// or restore pass re-applies a stamp it has just read (rsync -t, cp -p), which
// is exactly a request equal to the pre-read value.
func TestSetFileAttributes_TimestampEqualToPreReadIsStillWritten(t *testing.T) {
	cases := []struct {
		name  string
		field func(*metadata.FileAttr) time.Time
		set   func(time.Time) *metadata.SetAttrs
	}{
		{
			name:  "Atime",
			field: func(a *metadata.FileAttr) time.Time { return a.Atime },
			set:   func(v time.Time) *metadata.SetAttrs { return &metadata.SetAttrs{Atime: &v} },
		},
		{
			name:  "Mtime",
			field: func(a *metadata.FileAttr) time.Time { return a.Mtime },
			set:   func(v time.Time) *metadata.SetAttrs { return &metadata.SetAttrs{Mtime: &v} },
		},
		{
			name:  "CreationTime",
			field: func(a *metadata.FileAttr) time.Time { return a.CreationTime },
			set:   func(v time.Time) *metadata.SetAttrs { return &metadata.SetAttrs{CreationTime: &v} },
		},
		{
			// Ctime is decided by its own switch in the commit, so it needs its
			// own row here: the switch hands a held change time to the row and
			// an explicit one to the caller, and only the second is a request.
			name:  "Ctime",
			field: func(a *metadata.FileAttr) time.Time { return a.Ctime },
			set:   func(v time.Time) *metadata.SetAttrs { return &metadata.SetAttrs{Ctime: &v} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ws, handle := lostUpdateFixture(t, "t.bin", 0o644)
			root := rootAuth()

			// Read back rather than use a wall clock: sqlite keeps timestamps
			// as FILETIME ticks, so only a value that has been through the
			// store can be asked for again unchanged.
			before, err := svc.GetFile(root.Context, handle)
			require.NoError(t, err)
			stored := tc.field(&before.FileAttr)

			assertFired := windowPeer(t, ws, func() {
				_, hookErr := svc.SetFileAttributes(root, handle, tc.set(stored.Add(120*time.Second)))
				require.NoError(t, hookErr)
			})

			_, err = svc.SetFileAttributes(root, handle, tc.set(stored))
			require.NoError(t, err)
			assertFired()

			after, err := svc.GetFile(root.Context, handle)
			require.NoError(t, err)
			require.True(t, stored.Equal(tc.field(&after.FileAttr)),
				"%s = %v; want %v — the request equalled the value the call read, so the column "+
					"was never written and the peer's stamp survived",
				tc.name, tc.field(&after.FileAttr).UTC(), stored.UTC())
		})
	}
}

// modeDOSSparse is the high-word DOS attribute bit the SMB sparse-file FSCTL
// flips through ModeOrMask / ModeAndNotMask.
const modeDOSSparse = uint32(0x200000)

// The mask path carries DOS attribute bits and is a read-modify-write, so it
// has to compose with the row rather than replace it from a pre-read copy.
func TestSetFileAttributes_ModeOrMaskAlreadySetIsStillWritten(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "sparse.bin", 0o644)
	root := rootAuth()

	set := modeDOSSparse
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{ModeOrMask: &set})
	require.NoError(t, err)

	assertFired := windowPeer(t, ws, func() {
		clear := modeDOSSparse
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{ModeAndNotMask: &clear})
		require.NoError(t, hookErr)
	})

	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{ModeOrMask: &set})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.NotZero(t, after.Mode&modeDOSSparse,
		"SET_SPARSE reported success and the bit is clear: the OR left the working copy unchanged, "+
			"so the column was never written and the peer's clear survived")
}

// A chmod rewrites the mode's OWNER@/GROUP@/EVERYONE@ ACEs on the row it is
// committing to. That rewrite is keyed off the request, so on a build where the
// mode column is keyed off a diff the two disagree: the ACL is rewritten for
// the requested mode while the mode column keeps the peer's, and the row ends
// up granting a right its own mode bits deny.
func TestSetFileAttributes_ModeEqualToPreReadKeepsACLAndModeInStep(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "acl.bin", 0o644)
	root := rootAuth()

	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{ACL: &acl.ACL{ACEs: []acl.ACE{{
		Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
		Who:        "OWNER@",
		AccessMask: acl.ACE4_READ_DATA,
	}}}})
	require.NoError(t, err)

	assertFired := windowPeer(t, ws, func() {
		peer := uint32(0o400)
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &peer})
		require.NoError(t, hookErr)
	})

	same := uint32(0o644)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &same})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.NotNil(t, after.ACL)

	var ownerWrite bool
	for _, ace := range after.ACL.ACEs {
		if ace.Who == "OWNER@" && ace.Type == acl.ACE4_ACCESS_ALLOWED_ACE_TYPE &&
			ace.AccessMask&acl.ACE4_WRITE_DATA != 0 {
			ownerWrite = true
		}
	}
	require.True(t, ownerWrite,
		"the chmod's ACL rewrite did not land, ACEs=%+v", after.ACL.ACEs)
	require.NotZero(t, after.Mode&0o200,
		"the ACL was rewritten to grant the owner write for the requested 0644 while the mode "+
			"column kept the peer's %#o — the row grants what its own mode bits deny", after.Mode&0o7777)
}

// The POSIX setid strip is a read-modify-write too: it clears two bits, so it
// must be applied to the row rather than shipped as a whole mode computed from
// the pre-read copy, which would carry the peer's permission bits away with it.
func TestSetFileAttributes_SetidStripAppliesToTheCommittedRow(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "suid.bin", 0o4755)
	root := rootAuth()

	assertFired := windowPeer(t, ws, func() {
		peer := uint32(0o4700)
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &peer})
		require.NoError(t, hookErr)
	})

	newGID := uint32(42)
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{GID: &newGID})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint32(0o700), after.Mode&0o7777,
		"mode = %#o; want 0700 — the chown must strip setid from the row it is writing, not ship a "+
			"mode derived from the copy it read before the peer's chmod", after.Mode&0o7777)
	require.Equal(t, newGID, after.GID, "the chown itself must still land")
}

func nonRootAuth(uid, gid uint32) *metadata.AuthContext {
	ctx := rootAuth()
	ctx.Identity = &metadata.Identity{UID: metadata.Uint32Ptr(uid), GID: metadata.Uint32Ptr(gid)}
	return ctx
}

// The in-transaction ownership recheck exists because the ownership gate ran
// against a copy read before the transaction. Operations POSIX gates on write
// permission never consulted the owner, so a peer chown landing in the window
// invalidates nothing about them and must not turn them into EPERM.
func TestSetFileAttributes_WritePermittedOpSurvivesConcurrentChown(t *testing.T) {
	cases := []struct {
		name  string
		mode  uint32
		auth  func() *metadata.AuthContext
		attrs func() *metadata.SetAttrs
	}{
		{name: "MtimeNow", mode: 0o666, attrs: func() *metadata.SetAttrs {
			return &metadata.SetAttrs{MtimeNow: true}
		}},
		{name: "Truncate", mode: 0o666, attrs: func() *metadata.SetAttrs {
			size := uint64(8)
			return &metadata.SetAttrs{Size: &size}
		}},
		{
			// FileEndOfFileInformation carries the open handle's write grant.
			// POSIX denies this caller outright at mode 0600, so the handle is
			// demonstrably the only thing authorizing the write — and an open
			// handle's grant is not revoked by a later chown.
			name: "HandleAuthorizedSize",
			mode: 0o600,
			auth: func() *metadata.AuthContext {
				c := nonRootAuth(1000, 1000)
				c.WriteAuthorizedByHandle = true
				return c
			},
			attrs: func() *metadata.SetAttrs {
				size := uint64(8)
				return &metadata.SetAttrs{Size: &size}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ws, handle := lostUpdateFixture(t, "w.bin", tc.mode)
			root := rootAuth()

			owner := uint32(2000)
			_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &owner})
			require.NoError(t, err)

			assertFired := windowPeer(t, ws, func() {
				next := uint32(3000)
				_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &next})
				require.NoError(t, hookErr)
			})

			caller := nonRootAuth(1000, 1000)
			if tc.auth != nil {
				caller = tc.auth()
			}
			_, err = svc.SetFileAttributes(caller, handle, tc.attrs())
			require.NoError(t, err,
				"a peer chown turned a write-permission-gated %s into EPERM; its authorization "+
					"never read the file's owner", tc.name)
			assertFired()
		})
	}
}

// The other half of the same rule: a call that ownership authorized still has
// to be refused, or the gate and the write describe different files.
//
// A truncate and a utimes-to-now are gated on write permission when a stranger
// asks for them, but an owner reaches both without the write check ever running
// — the authorization switch lets ownership answer first. Exempting them for
// being write-permission-gated would therefore exempt a decision nothing but
// ownership ever made, which is the hole the recheck exists to close: mode 0600
// grants the caller nothing once the file belongs to someone else.
func TestSetFileAttributes_OwnerAuthorizedOpRefusedAfterConcurrentChown(t *testing.T) {
	cases := []struct {
		name  string
		auth  func() *metadata.AuthContext
		attrs func() *metadata.SetAttrs
	}{
		{name: "Chmod", attrs: func() *metadata.SetAttrs {
			m := uint32(0o640)
			return &metadata.SetAttrs{Mode: &m}
		}},
		{name: "MtimeNow", attrs: func() *metadata.SetAttrs {
			return &metadata.SetAttrs{MtimeNow: true}
		}},
		{name: "Truncate", attrs: func() *metadata.SetAttrs {
			size := uint64(0)
			return &metadata.SetAttrs{Size: &size}
		}},
		{
			// The same handle-carried write grant as the exempt table above,
			// but here the caller is the owner — so the authorization switch
			// let ownership answer first and the handle grant was never
			// consulted. Exempting the operation for carrying the flag would
			// exempt a decision only ownership ever made.
			name: "HandleAuthorizedSizeAsOwner",
			auth: func() *metadata.AuthContext {
				c := nonRootAuth(1000, 1000)
				c.WriteAuthorizedByHandle = true
				return c
			},
			attrs: func() *metadata.SetAttrs {
				size := uint64(0)
				return &metadata.SetAttrs{Size: &size}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ws, handle := lostUpdateFixture(t, "o.bin", 0o600)
			root := rootAuth()

			owner := uint32(1000)
			_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &owner})
			require.NoError(t, err)

			assertFired := windowPeer(t, ws, func() {
				next := uint32(3000)
				_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &next})
				require.NoError(t, hookErr)
			})

			caller := nonRootAuth(1000, 1000)
			if tc.auth != nil {
				caller = tc.auth()
			}
			_, err = svc.SetFileAttributes(caller, handle, tc.attrs())
			require.Error(t, err,
				"%s was authorized by ownership a peer chown has since moved, and mode 0600 grants "+
					"uid 1000 nothing on the new owner's file", tc.name)
			assertFired()

			var storeErr *metadata.StoreError
			require.ErrorAs(t, err, &storeErr)
			require.Equal(t, metadata.ErrPermissionDenied, storeErr.Code)
		})
	}
}

// Two EA writers naming different attributes must not lose each other's keys.
// SET_INFO builds a SetAttrs{EAMutations} and calls SetFileAttributes directly,
// so this path carries concurrent EA writes and cannot rely on xattr.go's lock.
func TestSetFileAttributes_ConcurrentEAWriterKeepsItsKey(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "ea.bin", 0o644)
	root := rootAuth()

	assertFired := windowPeer(t, ws, func() {
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{
			EAMutations: []metadata.EAMutation{{Name: "PEER", Value: []byte("p")}},
		})
		require.NoError(t, hookErr)
	})

	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{
		EAMutations: []metadata.EAMutation{{Name: "MINE", Value: []byte("m")}},
	})
	require.NoError(t, err)
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	mine, ok := after.LookupEA("MINE")
	require.True(t, ok, "this call's own EA is missing")
	require.Equal(t, []byte("m"), mine)
	peer, ok := after.LookupEA("PEER")
	require.True(t, ok,
		"the EA committed inside this call's window is gone: the map was replaced wholesale from "+
			"the copy read before the transaction instead of folded onto the row")
	require.Equal(t, []byte("p"), peer)
}

// Ownership is a request like any other. The code above only moves UID/GID when
// the request differs from what it read, which is exactly the test this whole
// change exists to stop using: a caller naming the owner it just read has asked
// for that owner, and a peer chown in the window must not be what stands.
func TestSetFileAttributes_ChownEqualToPreReadIsStillWritten(t *testing.T) {
	// Alone, the request also has to be what opens the transaction — keyed off
	// the move, a no-op chown committed nothing at all and the window hook
	// never even fired.
	for _, alsoChmod := range []bool{false, true} {
		name := "ChownOnly"
		if alsoChmod {
			name = "ChownWithChmod"
		}
		t.Run(name, func(t *testing.T) {
			svc, ws, handle := lostUpdateFixture(t, "c.bin", 0o644)
			root := rootAuth()

			owner := uint32(1000)
			_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &owner})
			require.NoError(t, err)

			assertFired := windowPeer(t, ws, func() {
				peer := uint32(2000)
				_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &peer})
				require.NoError(t, hookErr)
			})

			same := uint32(1000)
			attrs := &metadata.SetAttrs{UID: &same}
			if alsoChmod {
				m := uint32(0o640)
				attrs.Mode = &m
			}
			_, err = svc.SetFileAttributes(root, handle, attrs)
			require.NoError(t, err)
			assertFired()

			after, err := svc.GetFile(root.Context, handle)
			require.NoError(t, err)
			require.Equal(t, uint32(1000), after.UID,
				"chown to uid 1000 reported success and left uid %d: the request equalled the "+
					"value the call read, so the column was never written and the peer's chown "+
					"survived", after.UID)
		})
	}
}

// The ownership recheck invalidates on the fields the authorization actually
// read. Ownership is a UID comparison, so a peer chgrp leaves the caller the
// owner and refusing them is a spurious EPERM.
func TestSetFileAttributes_OwnerChmodSurvivesConcurrentChgrp(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "g.bin", 0o600)
	root := rootAuth()

	owner, group := uint32(1000), uint32(1000)
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &owner, GID: &group})
	require.NoError(t, err)

	assertFired := windowPeer(t, ws, func() {
		peer := uint32(9000)
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{GID: &peer})
		require.NoError(t, hookErr)
	})

	mode := uint32(0o640)
	_, err = svc.SetFileAttributes(nonRootAuth(1000, 1000), handle, &metadata.SetAttrs{Mode: &mode})
	require.NoError(t, err,
		"a peer chgrp turned the owner's chmod into EPERM: ownership is a UID comparison and the "+
			"caller still owns the file")
	assertFired()

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint32(0o640), after.Mode&0o7777, "the chmod itself must still land")
	require.Equal(t, uint32(9000), after.GID, "the peer's chgrp must still stand")
}

// The other side of that narrowing: an SGID grant is the one ownership-
// authorized decision that does read the file's group, so a peer chgrp does
// invalidate it and must still refuse.
func TestSetFileAttributes_SetgidChmodRefusedAfterConcurrentChgrp(t *testing.T) {
	svc, ws, handle := lostUpdateFixture(t, "sg.bin", 0o600)
	root := rootAuth()

	owner, group := uint32(1000), uint32(1000)
	_, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{UID: &owner, GID: &group})
	require.NoError(t, err)

	assertFired := windowPeer(t, ws, func() {
		peer := uint32(9000)
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{GID: &peer})
		require.NoError(t, hookErr)
	})

	mode := uint32(0o2640)
	_, err = svc.SetFileAttributes(nonRootAuth(1000, 1000), handle, &metadata.SetAttrs{Mode: &mode})
	require.Error(t, err,
		"the SGID grant was computed against a group the caller belonged to and a peer chgrp has "+
			"since moved the file out of it")
	assertFired()

	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrPermissionDenied, storeErr.Code)
}
