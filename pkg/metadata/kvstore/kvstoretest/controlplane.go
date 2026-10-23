package kvstoretest

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"iter"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

var ctx = context.Background()

// newStore initialises and opens a store over a fresh KV from o, behind a
// counting wrapper so a case can assert an operation's cost.
func newStore(t *testing.T, kvo Opener) (*kvstore.Store, *kv.Counting) {
	t.Helper()
	c := &kv.Counting{KV: kvo(t)}
	if err := kvstore.Init(ctx, c, uuid.New()); err != nil {
		t.Fatal(err)
	}
	s, err := kvstore.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	c.Take()
	return s, c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func all[T any](t *testing.T, seq iter.Seq2[T, error]) []T {
	t.Helper()
	var out []T
	for v, err := range seq {
		must(t, err)
		out = append(out, v)
	}
	return out
}

func user(name string, ids ...entity.ProtocolID) entity.User {
	return entity.User{Principal: entity.NewUser(), Name: name, IDs: ids}
}

func group(name string) entity.Group { return entity.Group{Principal: entity.NewGroup(), Name: name} }

func uid(n string) entity.ProtocolID { return entity.ProtocolID{Scheme: entity.SchemeUID, Value: n} }

func share(name, path string) entity.Share {
	return entity.Share{ID: entity.NewShareID(), Name: name, Path: path, Root: entity.NewFileID()}
}

func testUserRenameAndIDsMoveTheirRows(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u := user("alice", uid("1000"))
	must(t, cp.PutUser(ctx, u))

	u.Name, u.IDs = "alicia", entity.ProtocolIDs{uid("1001")}
	must(t, cp.PutUser(ctx, u))
	if _, err := p.ByName(ctx, entity.PrincipalUser, "alice"); !merrs.IsNotFoundError(err) {
		t.Errorf("old name still resolves: %v", err)
	}
	if got, err := p.ByName(ctx, entity.PrincipalUser, "alicia"); err != nil || got != u.Principal {
		t.Errorf("new name: %v, %v", got, err)
	}
	if _, err := p.ByProtocolID(ctx, uid("1000")); !merrs.IsNotFoundError(err) {
		t.Errorf("old uid still resolves: %v", err)
	}
	if got, err := p.ByProtocolID(ctx, uid("1001")); err != nil || got != u.Principal {
		t.Errorf("new uid: %v, %v", got, err)
	}
	// Both are free for another user now.
	must(t, cp.PutUser(ctx, user("alice", uid("1000"))))

	users := all(t, cp.Users(ctx, ""))
	if len(users) != 2 || users[0].Name != "alice" || users[1].Name != "alicia" {
		t.Fatalf("Users = %+v", users)
	}
	if !slices.Equal(users[1].IDs, u.IDs) {
		t.Errorf("IDs = %v", users[1].IDs)
	}
	if after := all(t, cp.Users(ctx, "alice")); len(after) != 1 || after[0].Name != "alicia" {
		t.Errorf("Users after alice = %+v", after)
	}
}

func testPrincipalUniqueness(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	must(t, cp.PutUser(ctx, user("alice", uid("1000"))))
	if err := cp.PutUser(ctx, user("alice")); !isExist(err) {
		t.Errorf("second alice: %v", err)
	}
	if err := cp.PutUser(ctx, user("bob", uid("1000"))); !isExist(err) {
		t.Errorf("uid 1000 twice: %v", err)
	}
	// A user and a group may share a name.
	must(t, cp.PutGroup(ctx, group("alice")))
	for _, bad := range []entity.User{
		user("carol", uid("01000")),
		user("carol", uid("x")),
		user("carol", entity.ProtocolID{Scheme: entity.SchemeGID, Value: "5"}),
		user("carol", uid("5"), uid("5")),
		user(""),
		user("carol", uid("0")),
		user("carol", uid("65534")),
	} {
		if err := cp.PutUser(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// gid 0 and 65534 are root's and anonymous's by rule, never a record's.
	for _, n := range []string{"0", "65534"} {
		g := group("carol-" + n)
		g.IDs = entity.ProtocolIDs{{Scheme: entity.SchemeGID, Value: n}}
		if err := cp.PutGroup(ctx, g); err == nil {
			t.Errorf("accepted group with gid %s", n)
		}
	}
}

func isExist(err error) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) && se.Code == merrs.ErrAlreadyExists
}

func testDeleteRemovesMembershipsAndGrants(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u, g, outer := user("alice"), group("staff"), group("all")
	sh := share("photos", "/photos")
	must(t, cp.PutUser(ctx, u))
	must(t, cp.PutGroup(ctx, g))
	must(t, cp.PutGroup(ctx, outer))
	must(t, cp.PutShare(ctx, sh))
	must(t, cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u.Principal}))
	must(t, cp.AddMember(ctx, entity.Membership{Group: outer.Principal, Member: g.Principal}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: outer.Principal, Access: entity.AccessRead}))
	if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 2 {
		t.Fatalf("Groups = %v", got)
	}
	if a, _ := p.Grant(ctx, sh.ID, u.Principal); a != entity.AccessRead {
		t.Fatalf("grant through nested groups = %v", a)
	}

	// Deleting the middle group cuts u off from outer and its grant.
	must(t, cp.DeleteGroup(ctx, g.Principal))
	if got := all(t, p.Groups(ctx, u.Principal)); len(got) != 0 {
		t.Errorf("Groups after DeleteGroup = %v", got)
	}
	if a, _ := p.Grant(ctx, sh.ID, u.Principal); a != entity.AccessNone {
		t.Errorf("grant after DeleteGroup = %v", a)
	}
	// Deleting outer drops its grant; a new group can take the name.
	must(t, cp.DeleteGroup(ctx, outer.Principal))
	again := group("all")
	must(t, cp.PutGroup(ctx, again))
	if a, _ := p.Grant(ctx, sh.ID, again.Principal); a != entity.AccessNone {
		t.Errorf("a new group inherited a deleted one's grant: %v", a)
	}
	must(t, cp.DeleteUser(ctx, u.Principal))
	if _, err := p.ByName(ctx, entity.PrincipalUser, "alice"); !merrs.IsNotFoundError(err) {
		t.Errorf("deleted user resolves: %v", err)
	}
	if err := cp.DeleteUser(ctx, u.Principal); !merrs.IsNotFoundError(err) {
		t.Errorf("second delete: %v", err)
	}
	if err := cp.AddMember(ctx, entity.Membership{Group: again.Principal, Member: u.Principal}); !merrs.IsNotFoundError(err) {
		t.Errorf("membership of a deleted user: %v", err)
	}
}

func testShares(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	list := func() uint64 {
		v, err := cp.ShareList(ctx)
		must(t, err)
		return v.Version
	}
	home := share("Home", "/home")
	must(t, cp.PutShare(ctx, home))
	if list() != 1 {
		t.Errorf("ShareList after create = %d", list())
	}
	for name, sh := range map[string]entity.Share{
		"name under the fold": share("HOME", "/other"),
		"same path":           share("other", "/home"),
		"descendant path":     share("alice", "/home/alice"),
		"ancestor path":       share("top", "/srv"),
	} {
		if name == "ancestor path" {
			must(t, cp.PutShare(ctx, share("deep", "/srv/a/b")))
			v := list()
			if err := cp.PutShare(ctx, sh); !isExist(err) {
				t.Errorf("%s: %v", name, err)
			}
			if list() != v {
				t.Errorf("%s: a refused put raised ShareList", name)
			}
			continue
		}
		if err := cp.PutShare(ctx, sh); !isExist(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := cp.PutShare(ctx, share("deeper", "/srv/a/b/c")); !isExist(err) {
		t.Errorf("below a share: %v", err)
	}
	// /homes is not below /home.
	must(t, cp.PutShare(ctx, share("homes", "/homes")))

	// Rename and re-path: the old name and path are free, ShareList moves.
	v := list()
	home.Name, home.Path = "house", "/house"
	must(t, cp.PutShare(ctx, home))
	if list() != v+1 {
		t.Errorf("ShareList after rename = %d, want %d", list(), v+1)
	}
	must(t, cp.PutShare(ctx, share("home", "/home")))
	// A put that changes nothing raises nothing.
	v = list()
	must(t, cp.PutShare(ctx, home))
	if list() != v {
		t.Error("an unchanged put raised ShareList")
	}
	moved := home
	moved.Root = entity.NewFileID()
	if err := cp.PutShare(ctx, moved); err == nil {
		t.Error("a share's root changed")
	}

	got := all(t, cp.Shares(ctx))
	if len(got) != 4 {
		t.Fatalf("Shares = %+v", got)
	}
	for _, sh := range got {
		if sh.State != entity.ShareEnabled {
			t.Errorf("%s starts %v", sh.Name, sh.State)
		}
	}
}

func testSetShareState(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh := share("photos", "/photos")
	must(t, cp.PutShare(ctx, sh))
	v, _ := cp.ShareList(ctx)
	must(t, cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced))
	if err := cp.SetShareState(ctx, sh.ID, entity.ShareEnabled, entity.ShareQuiesced); !merrs.IsConflictError(err) {
		t.Errorf("stale from: %v", err)
	}
	if w, _ := cp.ShareList(ctx); w.Version != v.Version+1 {
		t.Errorf("ShareList %d, want %d", w.Version, v.Version+1)
	}
	if got := all(t, cp.Shares(ctx)); got[0].State != entity.ShareQuiesced {
		t.Errorf("state %v", got[0].State)
	}
	if err := cp.SetShareState(ctx, entity.NewShareID(), entity.ShareEnabled, entity.ShareQuiesced); !merrs.IsNotFoundError(err) {
		t.Errorf("unknown share: %v", err)
	}
}

func testGrantVersions(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, p := s.ControlPlane(), s.Principals()
	u, g := user("alice"), group("staff")
	sh := share("photos", "/photos")
	must(t, cp.PutUser(ctx, u))
	must(t, cp.PutGroup(ctx, g))
	must(t, cp.PutShare(ctx, sh))
	must(t, cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u.Principal}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: g.Principal, Access: entity.AccessReadWrite}))
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessRead}))
	if a, _ := p.Grant(ctx, sh.ID, u.Principal); a != entity.AccessRead {
		t.Errorf("own grant = %v", a)
	}
	// Setting none removes the own grant; the group's decides again.
	must(t, cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: u.Principal, Access: entity.AccessNone}))
	if a, _ := p.Grant(ctx, sh.ID, u.Principal); a != entity.AccessReadWrite {
		t.Errorf("after removing the own grant = %v", a)
	}
	if err := cp.SetGrant(ctx, entity.ShareGrant{Share: entity.NewShareID(), Principal: u.Principal, Access: entity.AccessRead}); !merrs.IsNotFoundError(err) {
		t.Errorf("grant on a missing share: %v", err)
	}
	if err := cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: entity.NewUser(), Access: entity.AccessRead}); !merrs.IsNotFoundError(err) {
		t.Errorf("grant to a missing user: %v", err)
	}
}

// RFC 16 §4.2's quota records (qt, qx) and RFC 17 §5.6: limits on a user,
// a group or a project; when usage first went above the soft limit, kept
// until it falls back, restarted by a new soft limit; a principal's
// records go with it.
func testQuotas(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, c := s.ControlPlane(), s.Capacity()
	u, g := user("alice"), group("staff")
	sh := share("photos", "/photos")
	must(t, cp.PutUser(ctx, u))
	must(t, cp.PutGroup(ctx, g))
	must(t, cp.PutShare(ctx, sh))
	uq := entity.Quota{Share: sh.ID, Principal: u.Principal, Hard: entity.Limit{Bytes: 10 << 30}, Soft: entity.Limit{Bytes: 8 << 30},
		Grace: 7 * 24 * time.Hour, Advisory: entity.Limit{Files: 5000}}
	pq := entity.Quota{Share: sh.ID, Project: 7, Hard: entity.Limit{Files: 1000}}
	gq := entity.Quota{Share: sh.ID, Principal: g.Principal, Soft: entity.Limit{Bytes: 1 << 30}, Grace: time.Hour}
	for _, q := range []entity.Quota{uq, pq, gq} {
		must(t, cp.SetQuota(ctx, sh.ID, q))
	}
	var got []entity.Quota
	for q, err := range cp.Quotas(ctx, sh.ID) {
		must(t, err)
		got = append(got, q)
	}
	if len(got) != 3 || got[2] != pq {
		t.Fatalf("quotas %+v", got)
	}

	ut := kvstore.QuotaTarget{Principal: u.Principal}
	soft := func(above bool) time.Time {
		t.Helper()
		since, err := c.SoftExceeded(ctx, sh.ID, ut, above)
		must(t, err)
		return since
	}
	sinceNow := func() time.Time {
		t.Helper()
		q, since, err := c.Quota(ctx, sh.ID, ut)
		must(t, err)
		if q.Hard != uq.Hard {
			t.Errorf("quota %+v", q)
		}
		return since
	}
	if !sinceNow().IsZero() {
		t.Error("above the soft limit before anything said so")
	}
	first := soft(true)
	if first.IsZero() || !soft(true).Equal(first) || !sinceNow().Equal(first) {
		t.Errorf("the first crossing at %v is not kept", first)
	}
	uq.Hard.Bytes = 20 << 30 // another limit keeps the grace running
	must(t, cp.SetQuota(ctx, sh.ID, uq))
	if !sinceNow().Equal(first) {
		t.Error("a new hard limit restarted the grace")
	}
	uq.Soft.Bytes = 9 << 30 // a new soft limit restarts it
	must(t, cp.SetQuota(ctx, sh.ID, uq))
	if !sinceNow().IsZero() {
		t.Error("a new soft limit kept the old crossing")
	}
	soft(true)
	if !soft(false).IsZero() || !sinceNow().IsZero() {
		t.Error("falling back under did not clear the crossing")
	}

	// A quota that limits nothing is deleted.
	must(t, cp.SetQuota(ctx, sh.ID, entity.Quota{Project: 7}))
	if q, _, err := c.Quota(ctx, sh.ID, kvstore.QuotaTarget{Project: 7}); err != nil || !q.Unlimited() {
		t.Errorf("a deleted quota reads %+v, %v", q, err)
	}

	// A deleted principal's quotas go with it.
	gt := kvstore.QuotaTarget{Principal: g.Principal}
	if _, err := c.SoftExceeded(ctx, sh.ID, gt, true); err != nil {
		t.Fatal(err)
	}
	must(t, cp.DeleteGroup(ctx, g.Principal))
	if q, since, err := c.Quota(ctx, sh.ID, gt); err != nil || !q.Unlimited() || !since.IsZero() {
		t.Errorf("a deleted group's quota reads %+v since %v, %v", q, since, err)
	}

	// The share's own quota names neither a principal nor a project.
	must(t, cp.SetQuota(ctx, sh.ID, entity.Quota{Hard: entity.Limit{Bytes: 1 << 40}}))
	if q, _, err := c.Quota(ctx, sh.ID, kvstore.QuotaTarget{}); err != nil || q.Hard.Bytes != 1<<40 {
		t.Errorf("the share-wide quota reads %+v, %v", q, err)
	}
	got = got[:0]
	for q, err := range cp.Quotas(ctx, sh.ID) {
		must(t, err)
		got = append(got, q)
	}
	if len(got) != 2 || got[len(got)-1].Principal != (entity.Principal{}) || got[len(got)-1].Project != 0 {
		t.Errorf("quotas with the share's own last: %+v", got)
	}

	invalid := func(err error) bool { return isCode(err, merrs.ErrInvalidArgument) }
	for name, c := range map[string]struct {
		q    entity.Quota
		want func(error) bool
	}{
		"missing user":          {entity.Quota{Share: sh.ID, Principal: entity.NewUser(), Hard: entity.Limit{Files: 1}}, merrs.IsNotFoundError},
		"missing share":         {entity.Quota{Share: entity.NewShareID(), Project: 1, Hard: entity.Limit{Files: 1}}, merrs.IsNotFoundError},
		"principal and project": {entity.Quota{Share: sh.ID, Principal: u.Principal, Project: 1, Hard: entity.Limit{Files: 1}}, invalid},
		"negative":              {entity.Quota{Share: sh.ID, Project: 1, Hard: entity.Limit{Bytes: -1}}, invalid},
	} {
		if err := cp.SetQuota(ctx, c.q.Share, c.q); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// RFC 16 §4.2's qx rule: a changed soft limit restarts its grace at the
// change's time when usage is above the new limit, and clears it otherwise.
func testQuotaSoftChange(t *testing.T, kvo Opener) {
	f := newFS(t, kvo, entity.FoldIdentity)
	cp, c := f.s.ControlPlane(), f.s.Capacity()
	alice := f.member("alice", entity.Principal{})
	pub, _ := f.create(f.admin, f.root, "pub", directory(0o777))
	for _, name := range []string{"a", "b", "c"} {
		f.create(alice, pub, name, regular(0o644))
	}
	at := kvstore.QuotaTarget{Principal: alice.Principal}
	set := func(files int64) time.Time {
		t.Helper()
		must(t, cp.SetQuota(ctx, f.share.ID, entity.Quota{Principal: alice.Principal, Soft: entity.Limit{Files: files}, Grace: time.Hour}))
		_, since, err := c.Quota(ctx, f.share.ID, at)
		must(t, err)
		return since
	}
	if !set(5).IsZero() {
		t.Error("3 files under a soft limit of 5 recorded a crossing")
	}
	if set(2).IsZero() {
		t.Error("3 files over a new soft limit of 2 recorded no crossing")
	}
	if !set(10).IsZero() {
		t.Error("a soft limit raised above usage kept the crossing")
	}
}

func testExportPolicy(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh := share("photos", "/photos")
	must(t, cp.PutShare(ctx, sh))
	pol := entity.ExportPolicy{
		Share:   sh.ID,
		Flavors: []entity.AuthFlavor{entity.FlavorSys},
		Clients: []entity.ClientRule{{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessRead}},
		Squash:  entity.SquashRoot,
	}
	must(t, cp.PutExportPolicy(ctx, pol))
	got, err := cp.ExportPolicy(ctx, sh.ID)
	must(t, err)
	must(t, cp.PutExportPolicy(ctx, pol))
	again, _ := cp.ExportPolicy(ctx, sh.ID)
	if got.Version == 0 || again.Version <= got.Version {
		t.Errorf("versions %d then %d", got.Version, again.Version)
	}
	got.Version = 0
	if !slices.Equal(got.Clients, pol.Clients) || got.Squash != pol.Squash {
		t.Errorf("policy %+v", got)
	}
	pol.Clients[0].Access = entity.AccessAdmin
	if err := cp.PutExportPolicy(ctx, pol); err == nil {
		t.Error("a client rule granted admin")
	}
}

func testNetgroupsAndSettings(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	must(t, cp.PutNetgroup(ctx, entity.Netgroup{Name: "lab", Members: []string{"10.0.0.0/8"}}))
	must(t, cp.PutNetgroup(ctx, entity.Netgroup{Name: "office"}))
	if got := all(t, cp.Netgroups(ctx, "lab")); len(got) != 1 || got[0].Name != "office" {
		t.Errorf("Netgroups after lab = %+v", got)
	}
	must(t, cp.DeleteNetgroup(ctx, "lab"))
	if got := all(t, cp.Netgroups(ctx, "")); len(got) != 1 {
		t.Errorf("Netgroups = %+v", got)
	}

	inst := entity.SettingScope{Kind: entity.ScopeInstallation}
	shareScope := entity.SettingScope{Kind: entity.ScopeShare, ID: entity.NewShareID()}
	must(t, cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "a", Value: []byte("1")}))
	must(t, cp.PutSetting(ctx, entity.Setting{Scope: inst, Key: "a", Value: []byte("2")}))
	must(t, cp.PutSetting(ctx, entity.Setting{Scope: shareScope, Key: "a", Value: []byte("3")}))
	got := all(t, cp.Settings(ctx, inst))
	if len(got) != 1 || string(got[0].Value) != "2" || got[0].Generation != 2 {
		t.Errorf("installation settings = %+v", got)
	}
	if gen, _ := cp.SettingsGeneration(ctx); gen != 3 {
		t.Errorf("settings generation = %d", gen)
	}
	if err := cp.PutSetting(ctx, entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeShare}, Key: "a"}); err == nil {
		t.Error("share scope with no ID accepted")
	}
}

// A listing longer than one page resumes where the page ended.
func testListingPages(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	const n = 600
	for i := range n {
		must(t, cp.PutGroup(ctx, group(string(rune('a'+i/26/26%26))+string(rune('a'+i/26%26))+string(rune('a'+i%26)))))
	}
	got := all(t, cp.Groups(ctx, ""))
	if len(got) != n || !slices.IsSortedFunc(got, func(a, b entity.Group) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("%d groups, sorted %v", len(got), slices.IsSortedFunc(got, func(a, b entity.Group) int { return strings.Compare(a.Name, b.Name) }))
	}
}

// A share's fold rule, name rule and digest key are fixed at create.
func testShareBoundSettings(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	ci := share("docs", "/docs")
	ci.Fold = entity.FoldUpcase
	must(t, cp.PutShare(ctx, ci))
	must(t, cp.PutShare(ctx, share("src", "/src")))
	got := all(t, cp.Shares(ctx))
	if got[0].Fold != entity.FoldUpcase || got[0].Names != entity.NamesWindows || got[0].DigestKey == ([32]byte{}) {
		t.Errorf("case-insensitive share: %v %v", got[0].Fold, got[0].Names)
	}
	if got[1].Fold != entity.FoldIdentity || got[1].Names != entity.NamesPOSIX || got[1].DigestKey == got[0].DigestKey {
		t.Errorf("default share: %v %v", got[1].Fold, got[1].Names)
	}
	// A rename that names none of them keeps them.
	ci.Name, ci.Fold = "papers", 0
	must(t, cp.PutShare(ctx, ci))
	if got := all(t, cp.Shares(ctx)); got[0].Name != "papers" || got[0].Fold != entity.FoldUpcase || got[0].DigestKey == ([32]byte{}) {
		t.Errorf("after rename: %+v", got[0])
	}
	for name, mut := range map[string]func(*entity.Share){
		"fold":  func(s *entity.Share) { s.Fold = entity.FoldIdentity },
		"names": func(s *entity.Share) { s.Names = entity.NamesPOSIX },
		"key":   func(s *entity.Share) { s.DigestKey[0] ^= 1 },
	} {
		changed := ci
		mut(&changed)
		if err := cp.PutShare(ctx, changed); err == nil {
			t.Errorf("changing the %s was accepted", name)
		}
	}
}

// Every share gets a journal tag of its own, from 1: a put keeps it, and a
// share created after another's removal never gets the removed one's.
func testShareTags(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	a, b := share("a", "/a"), share("b", "/b")
	must(t, cp.PutShare(ctx, a))
	must(t, cp.PutShare(ctx, b))
	tags := func() map[entity.ShareID]uint32 {
		m := map[entity.ShareID]uint32{}
		for _, sh := range all(t, cp.Shares(ctx)) {
			m[sh.ID] = sh.Tag
		}
		return m
	}
	before := tags()
	if before[a.ID] == 0 || before[b.ID] == 0 || before[a.ID] == before[b.ID] {
		t.Fatalf("tags %v", before)
	}
	a.Name = "renamed"
	a.Tag = 77 // a put never writes the tag
	must(t, cp.PutShare(ctx, a))
	must(t, cp.DeleteShare(ctx, b.ID, entity.SnapshotsRefuse))
	c := share("c", "/c")
	must(t, cp.PutShare(ctx, c))
	after := tags()
	if after[a.ID] != before[a.ID] {
		t.Errorf("a put changed the tag %d -> %d", before[a.ID], after[a.ID])
	}
	if after[c.ID] <= before[b.ID] {
		t.Errorf("new share's tag %d does not follow the removed one's %d", after[c.ID], before[b.ID])
	}
}
