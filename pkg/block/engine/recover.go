package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// versionBlock is how far above the journal's next version start writes
// each served share's version limit (RFC 8 §2.7, proposal: 2²⁴ versions).
const versionBlock = 1 << 24

var (
	// errJournalLost refuses the shares of a device whose recorded journal
	// is missing or foreign: a lost device, until an operator acknowledges
	// the loss (RFC 8 §2.5).
	errJournalLost = errors.New("engine: the journal recorded for the device is missing or foreign")
	// errNotRecorded refuses the shares of a device the store records no
	// journal for, while its directory holds one: foreign, or of this
	// installation and not recorded (RFC 8 §2.5).
	errNotRecorded = errors.New("engine: no journal is recorded for the device")
	// errUnidentified refuses the shares of a device the store records no
	// journal for, while its directory holds segments and no format file
	// (RFC 28 §3.4).
	errUnidentified = errors.New("engine: the device holds segments and no format file")
	// errNotRaised fails a Start whose raise answered no incarnation for a
	// shard it serves (RFC 8 §2.5, step 2).
	errNotRaised = errors.New("engine: the start raise answered no incarnation for a served shard")
)

// opened is what start steps 1 and 2 leave for Start to publish: the
// journal, nil when its device was refused; the share table; the shares
// refused and the conditions raised; and the shards step 2 raised.
type opened struct {
	journal Journal
	table   bindings
	stats   Stats
	raised  []entity.ShardID
}

// openJournal is start step 1 for the one device (RFC 8 §2.5): it reads the
// journal identity, the shares' tags, version limits and floors from the
// store, opens the journal with the identity and the largest floor, forgets
// the tags of finished removals, gives each served share its handle, then
// writes every served share's version limit and passes it to the journal.
// Each thing it opens is pushed on u, for Start to close on a failure.
func (e *Engine) openJournal(ctx context.Context, shares []Share, u *undo) (opened, error) {
	dir := e.d.Devices[0]

	id, err := e.d.Lifecycle.JournalID(ctx, e.d.Node, entity.JournalDir(dir))
	if err != nil {
		return opened{}, fmt.Errorf("engine: start: read the journal identity of %s: %w", dir, err)
	}

	tags, err := e.d.Lifecycle.Tags(ctx)
	if err != nil {
		return opened{}, fmt.Errorf("engine: start: read the shares' tags: %w", err)
	}

	ids := floorShares(shares, tags)

	limits, err := e.d.Lifecycle.VersionLimits(ctx, ids)
	if err != nil {
		return opened{}, fmt.Errorf("engine: start: read the version limits: %w", err)
	}

	stored, err := e.d.Content.VersionFloor(ctx, ids)
	if err != nil {
		return opened{}, fmt.Errorf("engine: start: read the version floor: %w", err)
	}

	expect := journal.JournalID(id)
	floor := journal.VersionFromBytes(openFloor(stored, limits))

	j, err := e.d.Open(ctx, dir, floor, expect)
	if err != nil {
		return e.refuseDevice(dir, expect, shares, tags, err)
	}

	u.push(j.Close)

	if err := forgetFinished(ctx, j, tags); err != nil {
		return opened{}, err
	}

	o := opened{
		journal: j,
		table:   removedBindings(tags),
		stats:   newStats(),
	}

	var served []Share

	for _, s := range shares {
		if _, removed := o.table[s.ID]; removed {
			continue
		}

		// The journal's floor is at least the share's floor and limit.
		h, err := j.Share(s.Tag, s.Limit, floor)
		if err != nil {
			return opened{}, fmt.Errorf("engine: start: open share %v on %s: %w", s.ID, dir, err)
		}

		u.push(h.Close)

		b := &binding{
			share: s,
			j:     j,
			h:     h,
		}
		b.state.Store(uint32(s.State))
		o.table[s.ID] = b
		served = append(served, s)
	}

	if err := e.writeLimits(ctx, j, served); err != nil {
		return opened{}, err
	}

	return o, nil
}

// raiseAtStart is start step 2 (RFC 8 §2.5): it raises the incarnation of
// the shard of every share step 1 gave a handle, in the start form, as the
// restart lost the shards' open state (RFC 16 §3), and puts the incarnation
// it wrote on each share's binding. It is one store call, which splits the
// shards into transactions of at most K keys. The table is published only
// after it returns, so no write answers a verifier before the last of them
// commits. A failure fails Start, and the next Start raises every shard
// again, those an earlier attempt raised included. So does an answer that
// names no incarnation for a served shard: bound at zero, the shard would
// repeat its verifier and its grace instance.
func (e *Engine) raiseAtStart(ctx context.Context, o *opened) error {
	var shards []entity.ShardID

	for _, b := range o.table {
		if !b.removed {
			shards = append(shards, b.share.Shard)
		}
	}

	incs, err := e.d.Lifecycle.RaiseAtStart(ctx, shards)
	if err != nil {
		return fmt.Errorf("engine: start: raise the shards' incarnations: %w", err)
	}

	for _, b := range o.table {
		if b.removed {
			continue
		}

		inc, ok := incs[b.share.Shard]
		if !ok {
			return fmt.Errorf(
				"engine: start: shard %d of share %v: %w",
				b.share.Shard,
				b.share.ID,
				errNotRaised,
			)
		}

		b.incarnation = inc
	}

	for id := range incs {
		o.raised = append(o.raised, id)
	}

	return nil
}

// refuseDevice answers an Open that failed. A refusal it makes leaves every
// share on the device unattached and named, with the device and the cause,
// and Start goes on; a lost journal raises journal_lost. Any other error
// fails Start.
//
// decision: an error a check meets, rather than a refusal it makes, fails
// Start as any other failing step does, and stops every share of the node.
// Open returns it as itself (RFC 1 §3), so the engine cannot tell it from an
// error of recovery, and it gets the answer an I/O error at recovery gets
// (RFC 8 §2.5). Overturned with the rule for every error at open, once a
// journal that fails to open at start for any cause but a missing or
// foreign journal is left closed and retried, as a refused reopen is.
//
// ponytail: an Open that fails on a range of the journal it cannot read
// fails Start here, where RFC 8 §2.5 leaves only that journal's shares
// unattached and retries the open every 60 s. The journal names no error for
// it yet, so it cannot be told from a check's error; the ceiling is one
// unreadable sector stopping the node. Refuse it, raise
// journal_range_unreadable and retry the open, as the retried open of an
// unsuitable journal does, once the journal declares its own error for a
// range it cannot read.
//
// ponytail: with no journal recorded for the device, an empty directory fails
// Start as uninitialised and a directory of segments with no format file is
// refused by name, while RFC 8 §2.5 lets no share that is not recorded fail
// Start and RFC 28 §3.4 stops the start in every such case. The ceiling is a
// node with one device never initialised not starting at all. Refuse that
// device's shares instead, if the RFCs settle on that reading.
func (e *Engine) refuseDevice(
	dir string,
	expect journal.JournalID,
	shares []Share,
	tags map[entity.ShareID]entity.TagRecord,
	err error,
) (opened, error) {
	cause, lost := openRefusal(expect, err)
	if cause == nil {
		if expect == (journal.JournalID{}) && errors.Is(err, journal.ErrNoJournal) {
			return opened{}, fmt.Errorf("engine: start: %s holds no journal and none is recorded for it: not initialised: %w", dir, err)
		}

		return opened{}, fmt.Errorf("engine: start: open the journal of %s: %w", dir, err)
	}

	o := opened{
		table: removedBindings(tags),
		stats: newStats(),
	}

	for _, id := range deviceShares(shares, tags) {
		o.stats.Shares[id] = ShareStats{
			Device:  dir,
			Refused: cause,
		}
		e.d.Logger.Warn("share refused at start", "share", id, "device", dir, "err", cause)
	}

	if lost {
		c := Condition{
			Name: conditionJournalLost,
			Subject: Subject{
				Scope: ScopeJournal,
				ID:    hex.EncodeToString(expect[:]),
				Name:  dir,
			},
			Cause:  err,
			Detail: "the journal recorded for the device is missing or foreign; its shares wait for an operator to acknowledge the loss",
		}
		o.stats.Journals[dir] = JournalStats{
			Conditions: []Condition{c},
		}
	}

	return o, nil
}

// openRefusal is the cause the device's shares are refused with when Open
// failed with err under expect, and whether the journal is lost; nil when
// err is no refusal, and fails Start. With a journal recorded, one missing,
// foreign or unidentifiable is a lost device (RFC 8 §2.5, RFC 28 §3.4); with
// none recorded, a journal in the directory is not recorded, never a lost
// one, and segments with no format file are unidentified.
func openRefusal(expect journal.JournalID, err error) (cause error, lost bool) {
	recorded := expect != (journal.JournalID{})

	switch {
	case recorded && (errors.Is(err, journal.ErrNoJournal) ||
		errors.Is(err, journal.ErrIdentityMismatch) ||
		errors.Is(err, journal.ErrUnidentified)):
		return fmt.Errorf("%w: %w", errJournalLost, err), true
	case !recorded && errors.Is(err, journal.ErrIdentityMismatch):
		return fmt.Errorf("%w: %w", errNotRecorded, err), false
	case !recorded && errors.Is(err, journal.ErrUnidentified):
		return fmt.Errorf("%w: %w", errUnidentified, err), false
	}

	return nil, false
}

// forgetFinished runs Forget for every tag the journal names whose share's
// removal reads forgotten, before any share attaches (RFC 8 §2.5 step 1).
func forgetFinished(ctx context.Context, j Journal, tags map[entity.ShareID]entity.TagRecord) error {
	forgotten := map[journal.ShareTag]bool{}

	for _, r := range tags {
		if r.State == entity.TagForgotten {
			forgotten[journal.ShareTag(r.Tag)] = true
		}
	}

	for tag := range j.Stats().Shares {
		if !forgotten[tag] {
			continue
		}

		if err := j.Forget(ctx, tag); err != nil {
			return fmt.Errorf("engine: start: forget tag %d: %w", tag, err)
		}
	}

	return nil
}

// writeLimits writes every served share's version limit a block above the
// journal's next version, as a tracked max-write in transactions of at most
// K keys, and only once they have committed passes it to the journal (RFC 8
// §2.7).
//
// decision: a share the store records as being removed, whose removal has
// not reached its offload step, is attached, so its resumed removal can
// unbind it and drop its tag, but the store skips its limit, and with no
// other share served the journal is passed a limit no store key holds. That
// is safe because such a share draws no version: admission refuses every
// call into it from the removal's mark (RFC 16 §2.3.2, step 1), its unbind
// admits nothing new (RFC 8 §2.3), and its tag's forget record carries no
// version (RFC 28 §3.3). A share bound later writes the journal's limit as
// its own before it draws one. Overturned if a share being removed can draw
// a version: then the start write covers it too.
func (e *Engine) writeLimits(ctx context.Context, j Journal, served []Share) error {
	limit := startLimit(j.Stats().NextVersion)
	to := make(map[entity.ShareID]entity.JournalVersion, len(served))

	for _, s := range served {
		to[s.ID] = entity.JournalVersion(limit.Bytes())
	}

	if err := e.d.Lifecycle.RaiseVersionLimits(ctx, to); err != nil {
		return fmt.Errorf("engine: start: write the version limits: %w", err)
	}

	if err := j.SetVersionLimit(limit); err != nil {
		return fmt.Errorf("engine: start: set the version limit: %w", err)
	}

	return nil
}

// removedBindings is a table holding a removed binding for every share whose
// removal has reached its offload step: its tag and no handle, so a content
// call or a Bind naming it answers ErrStale (RFC 8 §12.1).
func removedBindings(tags map[entity.ShareID]entity.TagRecord) bindings {
	t := bindings{}

	for id, r := range tags {
		if r.State != entity.TagOffloaded && r.State != entity.TagForgotten {
			continue
		}

		t[id] = &binding{
			share: Share{
				ID:  id,
				Tag: journal.ShareTag(r.Tag),
			},
			removed: true,
		}
	}

	return t
}

// deviceShares is every share on the one device and not being removed: each
// share record that is served, and each share handed to Start.
func deviceShares(shares []Share, tags map[entity.ShareID]entity.TagRecord) []entity.ShareID {
	seen := map[entity.ShareID]bool{}

	var out []entity.ShareID

	add := func(id entity.ShareID) {
		if seen[id] {
			return
		}

		seen[id] = true
		out = append(out, id)
	}

	for _, s := range shares {
		if r, ok := tags[s.ID]; !ok || r.State == entity.TagServing {
			add(s.ID)
		}
	}

	for id, r := range tags {
		if r.State == entity.TagServing {
			add(id)
		}
	}

	return out
}

// floorShares is every share whose version floor and limit count in the
// floor the journal opens with: every share record of the installation,
// being removed included, and every share handed to Start.
func floorShares(shares []Share, tags map[entity.ShareID]entity.TagRecord) []entity.ShareID {
	out := make([]entity.ShareID, 0, len(tags)+len(shares))

	for id := range tags {
		out = append(out, id)
	}

	for _, s := range shares {
		if _, ok := tags[s.ID]; !ok {
			out = append(out, s.ID)
		}
	}

	return out
}

// openFloor is the floor the journal opens with: the largest of the shares'
// version floor, which the store reads as one number over every share, and
// every share's version limit (RFC 8 §2.5 step 1, RFC 28 §3.1).
//
// decision: the floor is the largest of them, one number for the journal,
// above every version issued before a crash; the journal's own doc names the
// smallest limit, which is also safe, while RFC 8 §2.5 and §2.7, RFC 28 §3.1
// and RFC 1 §3 take the largest, which also covers a share whose limit was
// raised ahead of the others. Overturned if those RFCs name another number.
func openFloor(floor entity.JournalVersion, limits map[entity.ShareID]entity.JournalVersion) entity.JournalVersion {
	top := floor

	for _, v := range limits {
		top = entity.MaxVersion(top, v)
	}

	return top
}

// startLimit is the version limit start writes for every served share: a
// block above the journal's next version, with a zero epoch (RFC 1 §4.3).
func startLimit(next journal.Version) journal.Version {
	return journal.Version{
		Counter: next.Counter + versionBlock,
	}
}

// undo is what Start has opened, closed in reverse when a step fails.
type undo struct {
	closers []func() error
}

// push records c to run when Start fails.
func (u *undo) push(c func() error) {
	u.closers = append(u.closers, c)
}

// unwind runs every closer in reverse and returns err, joined with any
// close that failed too.
func (u *undo) unwind(err error) error {
	errs := []error{err}

	for i := len(u.closers) - 1; i >= 0; i-- {
		if cerr := u.closers[i](); cerr != nil {
			errs = append(errs, cerr)
		}
	}

	if len(errs) == 1 {
		return err
	}

	return errors.Join(errs...)
}
