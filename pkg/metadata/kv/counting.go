package kv

import (
	"context"
	"iter"
	"sync"
)

// Counts is what transactions read and wrote: keys asked for by Get and
// Guard, ranges guarded, records a Scan yielded, keys written by Set and Delete.
type Counts struct {
	Gets, Guards, RangeGuards, Scanned, Sets, Deletes int
}

func (c *Counts) add(o Counts) {
	c.Gets += o.Gets
	c.Guards += o.Guards
	c.RangeGuards += o.RangeGuards
	c.Scanned += o.Scanned
	c.Sets += o.Sets
	c.Deletes += o.Deletes
}

// Counting wraps a KV and counts the keys each transaction touches, so a test
// can assert an operation's cost. Only an Update's final attempt counts:
// retries are the store's cost, not the operation's.
type Counting struct {
	KV
	mu    sync.Mutex
	total Counts
}

func (c *Counting) Update(ctx context.Context, fn func(Txn) error) error {
	var last Counts
	err := c.KV.Update(ctx, func(t Txn) error {
		last = Counts{}
		return fn(&countingTxn{Txn: t, c: &last})
	})
	c.record(last)
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

func (r *countingReader) Scan(prefix, after []byte) iter.Seq2[KeyValue, error] {
	return countScan(r.Reader.Scan(prefix, after), r.c)
}

type countingTxn struct {
	Txn
	c *Counts
}

func (t *countingTxn) Get(key []byte) ([]byte, error) {
	t.c.Gets++
	return t.Txn.Get(key)
}

func (t *countingTxn) Scan(prefix, after []byte) iter.Seq2[KeyValue, error] {
	return countScan(t.Txn.Scan(prefix, after), t.c)
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
