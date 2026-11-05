package kv

import (
	"context"
	"iter"
	"sync"
)

// Counts is what transactions read and wrote: keys asked for by Get, Guard and
// Versions, seeks by Before, ranges guarded, records a Scan yielded, keys
// written by Set and Delete. GuardOnly counts the Updates that guarded and wrote nothing: a
// backend may skip such a transaction's check, so no caller may make one.
type Counts struct {
	Gets, Guards, RangeGuards, Versions, Scanned, Sets, Deletes int
	Befores                                                     int
	GuardOnly                                                   int
}

func (c *Counts) add(o Counts) {
	c.Gets += o.Gets
	c.Guards += o.Guards
	c.RangeGuards += o.RangeGuards
	c.Versions += o.Versions
	c.Scanned += o.Scanned
	c.Sets += o.Sets
	c.Deletes += o.Deletes
	c.Befores += o.Befores
	c.GuardOnly += o.GuardOnly
}

// UpdateCounts is how many Updates committed or failed, and the most keys,
// set or deleted, one of them wrote in its final attempt.
type UpdateCounts struct {
	Updates    int
	MostWrites int
}

// Counting wraps a KV and counts the keys each transaction touches, so a test
// can assert an operation's cost. Only an Update's final attempt counts:
// retries are the store's cost, not the operation's.
type Counting struct {
	KV
	mu      sync.Mutex
	total   Counts
	updates UpdateCounts
}

func (c *Counting) Update(ctx context.Context, fn func(Txn) error) error {
	var last Counts
	err := c.KV.Update(ctx, func(t Txn) error {
		last = Counts{}
		return fn(&countingTxn{Txn: t, c: &last})
	})
	if last.Guards+last.RangeGuards > 0 && last.Sets+last.Deletes == 0 {
		last.GuardOnly = 1
	}
	c.record(last)

	c.mu.Lock()
	c.updates.Updates++
	c.updates.MostWrites = max(c.updates.MostWrites, last.Sets+last.Deletes)
	c.mu.Unlock()

	return err
}

func (c *Counting) View(ctx context.Context, fn func(Reader) error) error {
	var last Counts
	err := c.KV.View(ctx, func(r Reader) error {
		return fn(&countingReader{Reader: r, c: &last})
	})
	c.record(last)
	return err
}

// TakeUpdates returns the Update counts since the last TakeUpdates and
// resets them.
func (c *Counting) TakeUpdates() UpdateCounts {
	c.mu.Lock()

	defer c.mu.Unlock()

	u := c.updates
	c.updates = UpdateCounts{}

	return u
}

// Take returns the counts since the last Take and resets them.
func (c *Counting) Take() Counts {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.total
	c.total = Counts{}
	return t
}

func (c *Counting) record(n Counts) {
	c.mu.Lock()
	c.total.add(n)
	c.mu.Unlock()
}

type countingReader struct {
	Reader
	c *Counts
}

func (r *countingReader) Get(key []byte) ([]byte, error) {
	r.c.Gets++
	return r.Reader.Get(key)
}

// GetMany counts a read per key, as that many Gets would: a key count stays
// the same whichever way the store reads.
func (r *countingReader) GetMany(keys [][]byte) ([][]byte, error) {
	r.c.Gets += len(keys)
	return r.Reader.GetMany(keys)
}

func (r *countingReader) Versions(keys [][]byte) ([]uint64, error) {
	r.c.Versions += len(keys)
	return r.Reader.Versions(keys)
}

func (r *countingReader) Scan(prefix, after []byte) iter.Seq2[KeyValue, error] {
	return countScan(r.Reader.Scan(prefix, after), r.c)
}

func (r *countingReader) Before(prefix, key []byte) (KeyValue, bool, error) {
	r.c.Befores++
	return r.Reader.Before(prefix, key)
}

type countingTxn struct {
	Txn
	c *Counts
}

func (t *countingTxn) Get(key []byte) ([]byte, error) {
	t.c.Gets++
	return t.Txn.Get(key)
}

func (t *countingTxn) GetMany(keys [][]byte) ([][]byte, error) {
	t.c.Gets += len(keys)
	return t.Txn.GetMany(keys)
}

func (t *countingTxn) Scan(prefix, after []byte) iter.Seq2[KeyValue, error] {
	return countScan(t.Txn.Scan(prefix, after), t.c)
}

func (t *countingTxn) Before(prefix, key []byte) (KeyValue, bool, error) {
	t.c.Befores++
	return t.Txn.Before(prefix, key)
}

func (t *countingTxn) Versions(keys [][]byte) ([]uint64, error) {
	t.c.Versions += len(keys)
	return t.Txn.Versions(keys)
}

func (t *countingTxn) Guard(key []byte) error {
	t.c.Guards++
	return t.Txn.Guard(key)
}

func (t *countingTxn) GuardRange(prefix []byte) error {
	t.c.RangeGuards++
	return t.Txn.GuardRange(prefix)
}

func (t *countingTxn) Set(key, value []byte) error {
	t.c.Sets++
	return t.Txn.Set(key, value)
}

func (t *countingTxn) Delete(key []byte) error {
	t.c.Deletes++
	return t.Txn.Delete(key)
}

func countScan(s iter.Seq2[KeyValue, error], c *Counts) iter.Seq2[KeyValue, error] {
	return func(yield func(KeyValue, error) bool) {
		for kv, err := range s {
			if err == nil {
				c.Scanned++
			}
			if !yield(kv, err) {
				return
			}
		}
	}
}
