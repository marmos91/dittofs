package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// syncBuffer is a log sink safe for the loop's goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stepClock is a clock moved only by Advance, whose After fires once the
// clock reaches its deadline.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stepTimer
}

type stepTimer struct {
	at time.Time
	c  chan struct{}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Elapsed() time.Duration { return c.Now().Sub(time.Time{}) }

func (c *stepClock) After(d time.Duration) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	if d <= 0 {
		close(ch)
		return ch
	}
	c.timers = append(c.timers, stepTimer{c.now.Add(d), ch})
	return ch
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			kept = append(kept, tm)
		} else {
			close(tm.c)
		}
	}
	c.timers = kept
}

// scripted runs a loop over a test clock with one item answering answers in
// turn, and returns a step that advances the clock by one backoff and waits
// until the item has run once more and is back on the loop, or finished.
func scripted(t *testing.T, answers ...answer) (l *removalLoop, f entity.FileID, step func(), log *syncBuffer) {
	t.Helper()
	clock := &stepClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log = &syncBuffer{}
	e := &Engine{d: Deps{Clock: clock, Logger: slog.New(slog.NewTextHandler(log, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		}}}
	l = newRemovalLoop(e)
	go l.run()
	t.Cleanup(l.close)
	f = entity.NewFileID()
	var runs atomic.Int32
	l.enqueue(&loopItem{b: &binding{}, f: f, kind: entity.RemoveTruncate, run: func(context.Context) answer {
		n := runs.Add(1)
		return answers[n-1]
	}})
	steps := int32(0)
	settled := func(n int32) bool { // run n has finished and the item is back, or gone
		k, _ := l.pending()
		return runs.Load() == n && (k == 1 || answers[n-1] == itemDone)
	}
	wait := func(n int32) {
		deadline := time.Now().Add(5 * time.Second)
		for !settled(n) {
			if time.Now().After(deadline) {
				t.Fatalf("the item never finished run %d", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	step = func() {
		t.Helper()
		steps++
		if steps > 1 {
			wait(steps - 1)
			// One backoff at a time, until the run starts: the loop may arm
			// its timer after an Advance, which that Advance then missed.
			deadline := time.Now().Add(5 * time.Second)
			for runs.Load() < steps {
				if time.Now().After(deadline) {
					t.Fatalf("the item never started run %d", steps)
				}
				clock.Advance(loopBackoff)
				time.Sleep(time.Millisecond)
			}
		}
		wait(steps)
	}
	return l, f, step, log
}

// An item is removal_stalled after its fifth failure in a row, not its
// fourth, and the run that finishes it clears the condition.
func TestAnItemStallsAfterFiveFailures(t *testing.T) {
	l, f, step, log := scripted(t, itemRetry, itemRetry, itemRetry, itemRetry, itemRetry, itemDone)
	for range 4 {
		step()
	}
	if l.stalledOn(f) {
		t.Fatal("stalled after four failures")
	}
	step()
	if !l.stalledOn(f) {
		t.Fatal("not stalled after five failures")
	}
	step()
	if !strings.Contains(log.String(), "removal_stalled cleared") {
		t.Errorf("the run that finished it logged no clearing: %s", log.String())
	}
}

// A run held by something outside the item counts toward no stall.
func TestAnUncountedRetryLeavesTheCount(t *testing.T) {
	answers := []answer{itemRetry, itemRetry, itemRetry, itemRetry}
	for range 6 {
		answers = append(answers, itemRetryUncounted)
	}
	l, f, step, _ := scripted(t, append(answers, itemDone)...)
	for range 10 {
		step()
	}
	if l.stalledOn(f) {
		t.Error("stalled after four counted failures and six uncounted")
	}
}

// A share dropped while one of its items runs ends that item when its run
// returns, rather than putting it back, and no item of the share is taken
// after.
func TestADroppedSharesRunningItemIsEnded(t *testing.T) {
	clock := &stepClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e := &Engine{d: Deps{Clock: clock, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		}}}
	l := newRemovalLoop(e)
	go l.run()
	t.Cleanup(l.close)
	s := entity.NewShareID()
	b := &binding{share: Share{ID: s}}
	running, finish := make(chan struct{}), make(chan struct{})
	var ended atomic.Int32
	l.enqueue(&loopItem{b: b, f: entity.NewFileID(), kind: entity.RemoveTruncate,
		run: func(context.Context) answer { close(running); <-finish; return itemRetry },
		end: func() { ended.Add(1) }})
	<-running
	l.dropShare(s)
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for ended.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the running item of the dropped share was never ended")
		}
		time.Sleep(time.Millisecond)
	}
	if n, _ := l.pending(); n != 0 {
		t.Errorf("%d items left on the loop", n)
	}
	l.enqueue(&loopItem{b: b, f: entity.NewFileID(), kind: entity.RemoveTruncate,
		run: func(context.Context) answer { return itemDone }, end: func() { ended.Add(1) }})
	if n := ended.Load(); n != 2 {
		t.Errorf("an item enqueued for the dropped share: ended %d times in all, want it ended at once", n)
	}
}
