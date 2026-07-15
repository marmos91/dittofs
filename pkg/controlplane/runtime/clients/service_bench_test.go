package clients

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// lockRegistry models the old UpdateActivity: a process-wide write lock taken on
// every request just to bump a timestamp. It exists only to produce the "before"
// number for BenchmarkUpdateActivity.
type lockRegistry struct {
	mu      sync.RWMutex
	clients map[string]*ClientRecord
}

func (r *lockRegistry) updateActivity(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.clients[clientID]; ok {
		rec.LastActivity = time.Now()
	}
}

const benchClients = 64

func benchIDs() []string {
	ids := make([]string, benchClients)
	for i := range ids {
		ids[i] = "client-" + strconv.Itoa(i)
	}
	return ids
}

// BenchmarkUpdateActivity_WriteLock is the "before": every bump serializes on a
// single write lock. Throughput collapses as goroutines contend (lock convoy).
func BenchmarkUpdateActivity_WriteLock(b *testing.B) {
	ids := benchIDs()
	r := &lockRegistry{clients: make(map[string]*ClientRecord, benchClients)}
	for _, id := range ids {
		r.clients[id] = &ClientRecord{ClientID: id, LastActivity: time.Now()}
	}

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.updateActivity(ids[i%benchClients])
			i++
		}
	})
}

// BenchmarkUpdateActivity is the "after": a shared read lock plus a per-client
// atomic store, so concurrent bumps no longer convoy.
func BenchmarkUpdateActivity(b *testing.B) {
	ids := benchIDs()
	r := NewRegistry(time.Hour)
	for _, id := range ids {
		r.Register(&ClientRecord{ClientID: id})
	}

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.UpdateActivity(ids[i%benchClients])
			i++
		}
	})
}
