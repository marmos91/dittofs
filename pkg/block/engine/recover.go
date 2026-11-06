package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

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
)

// opened is what start step 1 leaves for Start to publish: the journal with
// its version limit, nil when its device was refused; the share table; and
// the shares refused and the conditions raised.
type opened struct {
	journal *versionLimit
	table   bindings
	stats   Stats
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

	vl := &versionLimit{
		j: j,
	}
	o := opened{
		journal: vl,
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
			vl:    vl,
			h:     h,
		}
		b.state.Store(uint32(s.State))
		o.table[s.ID] = b
		served = append(served, s)
	}

	limit, err := e.writeLimits(ctx, j, served)
	if err != nil {
		return opened{}, err
	}

	vl.last.Store(limit.Counter)

	return o, nil
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
// §2.7). It returns the limit it passed.
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
func (e *Engine) writeLimits(
	ctx context.Context,
	j Journal,
	served []Share,
) (journal.Version, error) {
	limit := startLimit(j.Stats().NextVersion, e.block)
	to := make(map[entity.ShareID]entity.JournalVersion, len(served))

	for _, s := range served {
		to[s.ID] = entity.JournalVersion(limit.Bytes())
	}

	if err := e.d.Lifecycle.RaiseVersionLimits(ctx, to); err != nil {
		return journal.Version{}, fmt.Errorf("engine: start: write the version limits: %w", err)
	}

	if err := j.SetVersionLimit(limit); err != nil {
		return journal.Version{}, fmt.Errorf("engine: start: set the version limit: %w", err)
	}

	return limit, nil
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
func startLimit(next journal.Version, block uint64) journal.Version {
	return journal.Version{
		Counter: limitTarget(0, next.Counter, block),
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

// The version limit while the journal serves (RFC 8 §2.7).
const (
	rateWindow          = 60               // the seconds the peak version rate looks back over
	fenceAfter          = 30 * time.Second // the self-fence's window (RFC 8 §11.2)
	triggerSampling     = time.Second      // the trigger's own sampling
	defaultExistenceAge = time.Second      // RFC 13's proposed existence_age, with no settings
	retryFirst          = 10 * time.Millisecond
	retryMost           = time.Second
)

// The states of a journal's raise: none running, one running, and one running
// with a wake that came during it.
const (
	raiseIdle int32 = iota
	raiseRunning
	raisePending
)

// errLimitRefused is a raise whose limit the journal refused to take: no
// retry changes it.
var errLimitRefused = errors.New("engine: the journal refused the version limit")

// versionLimit is one journal's version limit while it serves: the limit last
// passed with SetVersionLimit, the versions drawn in each second, the raise
// lock, and the state of its raise.
type versionLimit struct {
	j    Journal
	last atomic.Uint64 // the counter of the limit last passed with SetVersionLimit
	rate rateBuckets

	// lock is the raise lock: a raise holds it from its read of the counter
	// to its SetVersionLimit, and an attach from its limit write to its
	// publish.
	lock  sync.Mutex
	state atomic.Int32 // raiseIdle, raiseRunning or raisePending

	// refused is set once the journal refuses a limit: no raise changes
	// that, so no version wakes a raise again until the journal is opened
	// anew.
	refused atomic.Bool
}

// raises is the version limit raises running: the context they run under,
// which stopping cancels, and the group that joins them. count and started,
// nil in production, count the raise goroutines alive and every one started
// once a test sets them.
type raises struct {
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	count   *atomic.Int32
	started *atomic.Int64
}

// rateBuckets counts the versions drawn in each of the last rateWindow
// seconds. Each bucket packs the second it counts, in the high 32 bits, and
// its count, in the low 32, so one compare-and-swap moves it to a new second
// and counts there, with no lock on the request path (RFC 8 §1.1).
type rateBuckets struct {
	b [rateWindow]atomic.Uint64
}

// add counts one version drawn in second sec and returns the peak.
func (r *rateBuckets) add(sec uint64) uint64 {
	slot := &r.b[sec%rateWindow]

	for {
		old := slot.Load()
		n := uint64(1)

		if old>>32 == sec {
			n = old&math.MaxUint32 + 1
		}

		if slot.CompareAndSwap(old, sec<<32|n) {
			break
		}
	}

	return r.peak(sec)
}

// peak is the highest count of versions drawn in any one second of the
// rateWindow seconds up to sec.
func (r *rateBuckets) peak(sec uint64) uint64 {
	var top uint64

	for i := range r.b {
		v := r.b[i].Load()
		at := v >> 32

		if at > sec || at+rateWindow <= sec {
			continue
		}

		top = max(top, v&math.MaxUint32)
	}

	return top
}

// headroom is how many versions the journal can still issue below limit
// once it has issued v: a journal issues no version at or above its limit
// (RFC 1 §4.3), so the next is v+1 and the last limit-1, as the journal's
// Stats reports its versions left.
func headroom(limit, v uint64) uint64 {
	if v+1 >= limit {
		return 0
	}

	return limit - v - 1
}

// limitTarget is the limit a raise writes: a block above the larger of the
// limit last passed and the journal's next version. From the limit alone, a
// counter an attach moved far above it would leave the journal refusing
// writes for many raises; from the counter alone, a rate term above a block
// would raise nothing while versions are left.
func limitTarget(last, next, block uint64) uint64 {
	return max(last, next) + block
}

// due reports whether left versions are fewer than the trigger: the larger of
// half a block and the peak version rate times the margin (RFC 8 §2.7).
func due(left, peak uint64, margin time.Duration, block uint64) bool {
	secs := uint64(margin / time.Second)
	frac := uint64(margin % time.Second)
	rate := peak*secs + peak*frac/uint64(time.Second)

	return left < max(block/2, rate)
}

// margin is how long the headroom must last at the peak rate: the
// self-fence's 30 s, plus the existence age, since the fence can count from a
// commit up to one existence age before the stall, plus the trigger's own
// sampling (RFC 8 §2.7). It reads the settings at each check, so a changed
// existence age applies to the next version.
func (e *Engine) margin() time.Duration {
	age := defaultExistenceAge

	if s := e.settings.Load(); s != nil {
		age = s.ExistenceAge
	}

	return fenceAfter + age + triggerSampling
}

// second is the clock's second now, the bucket a version drawn now counts in.
func (e *Engine) second() uint64 {
	return uint64(e.d.Clock.Elapsed() / time.Second)
}

// issued counts version v, just drawn from vl's journal, and wakes the
// journal's raise when the versions left are fewer than the trigger. It is
// evaluated on every version issued, never on a timer, and waits for
// nothing: the raise runs on its own goroutine, so the request that drew v
// never waits for a transaction (RFC 8 §1.1, §2.7). A journal that refused a
// limit wakes no raise again.
//
// decision: the headroom is sized by time, not by count alone, so that the
// version limit does not refuse writes before the 30 s self-fence would at
// any rate the last minute has seen. The block size is a proposal;
// overturned by the sustained-rate stall check failing at a measured peak
// rate, which then sets the block.
//
// ponytail: the trigger looks back one minute, so a burst at a stall's start
// faster than the larger of about 280 000 versions a second (half a block
// over 30 s) and 1.07 times the last minute's peak (32 s of it over 30 s)
// spends the headroom before the fence: 500 000 a second after a minute at
// 100 000 is refused retry-later about 13 s before the fence would. The cost
// is a retry, never a failure. Raise by a larger block, or size the margin
// from the device's peak IOPS rather than the last minute, when the burst
// check is refused at a rate a deployment reaches.
func (e *Engine) issued(vl *versionLimit, v journal.Version) {
	peak := vl.rate.add(e.second())

	if vl.refused.Load() {
		return
	}

	if !due(headroom(vl.last.Load(), v.Counter), peak, e.margin(), e.block) {
		return
	}

	e.wake(vl)
}

// wake starts vl's raise, or, when one runs, marks it to run once more, so
// one raise runs at a time per journal and a wake during it is not lost.
func (e *Engine) wake(vl *versionLimit) {
	for {
		switch vl.state.Load() {
		case raiseIdle:
			if !vl.state.CompareAndSwap(raiseIdle, raiseRunning) {
				continue
			}

			if c := e.raising.count; c != nil {
				c.Add(1)
			}

			if c := e.raising.started; c != nil {
				c.Add(1)
			}

			e.raising.wg.Add(1)

			go e.raiseLoop(vl)

			return
		case raiseRunning:
			if vl.state.CompareAndSwap(raiseRunning, raisePending) {
				return
			}
		default:
			return
		}
	}
}

// again ends one pass of vl's raise: true when a wake came during it, so the
// raise runs once more, and false, leaving no raise running, otherwise.
func (vl *versionLimit) again() bool {
	for {
		if vl.state.CompareAndSwap(raisePending, raiseRunning) {
			return true
		}

		if vl.state.CompareAndSwap(raiseRunning, raiseIdle) {
			return false
		}
	}
}

// raiseLoop is vl's raise: it raises, runs once more for a wake that came
// meanwhile, and retries a raise that failed until it commits, waiting
// between attempts, off the request path (RFC 8 §11.2). A limit the journal
// refused is not retried: no retry changes it.
//
// decision: a failed raise waits 10 ms before its first retry, doubling to
// at most 1 s, and starts again from 10 ms after a raise that commits. RFC 8
// does not set the wait; it bounds how hard a failing store is called and
// how soon a returning one is used. Overturned if RFC 8 sets a retry wait for
// the owners that retry their write transactions.
func (e *Engine) raiseLoop(vl *versionLimit) {
	defer e.raising.wg.Done()

	if c := e.raising.count; c != nil {
		defer c.Add(-1)
	}

	ctx := e.raising.ctx
	wait := retryFirst
	extra := false

	for {
		// A raise woken before the journal refused a limit runs no store
		// call: the refusal persists, so it ends here, logging nothing.
		if vl.refused.Load() {
			vl.state.Store(raiseIdle)

			return
		}

		// A wake that came before this pass reads the counter is taken
		// here, so the pass covers it and the end of the pass does not run
		// the raise once more for it. This only saves an extra pass: a wake
		// left pending is seen by again either way, so none is lost.
		vl.state.CompareAndSwap(raisePending, raiseRunning)

		// The pass a wake during the last raise asked for raises only if
		// the trigger still holds: while a raise runs the limit has not
		// moved, so every version issued meanwhile wakes it, and raising
		// for each of those would spend a block and a store transaction
		// the versions left do not need (RFC 8 §2.7).
		if extra && !e.stillDue(vl) {
			if vl.state.CompareAndSwap(raiseRunning, raiseIdle) {
				return
			}

			continue
		}

		extra = false

		err := e.raiseOnce(ctx, vl)
		switch {
		case err == nil:
			wait = retryFirst

			if !vl.again() {
				return
			}

			extra = true

			continue
		case errors.Is(err, errLimitRefused):
			e.d.Logger.Error("version limit raise refused by the journal; not retried", "err", err)

			// Going idle drops a wake that came during this raise. The
			// refusal persists, so a retry would be refused again.
			// raiseLocked has marked the journal refused: a version issued
			// after the mark wakes no raise, and at most a raise woken just
			// before it runs, ending at the top of its loop with no store
			// call and no log.
			vl.state.Store(raiseIdle)

			return
		case ctx.Err() != nil:
			vl.state.Store(raiseIdle)

			return
		}

		e.d.Logger.Warn("version limit raise failed; retrying", "wait", wait, "err", err)

		if !e.retryWait(ctx, wait) {
			vl.state.Store(raiseIdle)

			return
		}

		wait = min(2*wait, retryMost)
	}
}

// stillDue reports whether vl's journal is still within the trigger, read
// from the limit last passed, the counter and the settings now.
func (e *Engine) stillDue(vl *versionLimit) bool {
	next := vl.j.Stats().NextVersion.Counter
	left := headroom(vl.last.Load(), max(next, 1)-1)

	return due(left, vl.rate.peak(e.second()), e.margin(), e.block)
}

// retryWait waits d on the clock before a retry, and reports false when ctx
// ends first.
func (e *Engine) retryWait(ctx context.Context, d time.Duration) bool {
	select {
	case <-e.d.Clock.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// raiseOnce is one raise of vl under its raise lock.
func (e *Engine) raiseOnce(ctx context.Context, vl *versionLimit) error {
	vl.lock.Lock()

	defer vl.lock.Unlock()

	return e.raiseLocked(ctx, vl, nil)
}

// raiseLocked raises the limit of every share served on vl's journal, and of
// the shares in also, to a block above the larger of the limit last passed
// and the journal's next version, in one call the store splits into
// transactions of at most K keys, and passes it to the journal only once it
// has committed (RFC 8 §2.7, RFC 1 §4.3). The limit keeps a zero epoch. The
// caller holds the raise lock.
func (e *Engine) raiseLocked(ctx context.Context, vl *versionLimit, also []entity.ShareID) error {
	next := vl.j.Stats().NextVersion
	mark := journal.Version{
		Counter: limitTarget(vl.last.Load(), next.Counter, e.block),
	}
	v := entity.JournalVersion(mark.Bytes())
	to := map[entity.ShareID]entity.JournalVersion{}

	for id, b := range e.table() {
		if b.removed || b.vl != vl {
			continue
		}

		to[id] = v
	}

	for _, id := range also {
		to[id] = v
	}

	if err := e.d.Lifecycle.RaiseVersionLimits(ctx, to); err != nil {
		return err
	}

	if err := vl.j.SetVersionLimit(mark); err != nil {
		vl.refused.Store(true)

		return fmt.Errorf("%w: %w", errLimitRefused, err)
	}

	vl.last.Store(mark.Counter)

	return nil
}

// attachLimit writes share s's limit at the limit last passed, or, when the
// journal's counter its Share raised leaves fewer versions than the trigger,
// raises every served share's limit and s's itself first (RFC 8 §2.5). The
// caller holds the raise lock.
func (e *Engine) attachLimit(ctx context.Context, s Share, vl *versionLimit) error {
	if e.stillDue(vl) {
		if err := e.raiseLocked(ctx, vl, []entity.ShareID{s.ID}); err != nil {
			if errors.Is(err, errLimitRefused) {
				return e.journalErr(err)
			}

			return storeErr(err)
		}

		return nil
	}

	to := map[entity.ShareID]entity.JournalVersion{
		s.ID: entity.JournalVersion(journal.Version{Counter: vl.last.Load()}.Bytes()),
	}

	if err := e.d.Lifecycle.RaiseVersionLimits(ctx, to); err != nil {
		return storeErr(err)
	}

	return nil
}

// stopRaises cancels the version limit raises running and joins them, then
// readies a new context for a later Start's raises.
func (e *Engine) stopRaises() error {
	e.raising.cancel()
	e.raising.wg.Wait()
	e.raising.ctx, e.raising.cancel = context.WithCancel(context.Background())

	return nil
}
