// Package lock provides a bounded notification queue for directory change events.
//
// The NotificationQueue collects directory change notifications for clients
// holding directory delegations. When the queue reaches its capacity, it
// discards all queued events, sets an overflow flag, and drops all subsequent
// pushes. Callers detect overflow via the boolean returned by Drain() and
// should perform a full directory rescan when overflow is true.
//
// This is used by directory delegation holders to receive fine-grained
// change notifications (add, remove, rename) or, on overflow, a signal
// that a full directory rescan is required.
package lock

import (
	"sync"
)

const (
	// DefaultNotificationQueueCapacity is the default maximum number of
	// queued directory change notifications before overflow.
	DefaultNotificationQueueCapacity = 1024

	// notificationFlushThreshold is the number of events that triggers
	// a signal on the flush channel.
	notificationFlushThreshold = 100
)

// DirNotification represents a single directory change event.
type DirNotification struct {
	// ChangeType is the type of directory change.
	ChangeType DirChangeType

	// EntryName is the name of the affected entry.
	EntryName string

	// OldName is the previous name (for rename operations).
	OldName string

	// NewName is the new name (for rename operations).
	NewName string
}

// NotificationQueue is a bounded, thread-safe queue for directory change events.
//
// When the queue reaches its capacity, further pushes set an overflow flag
// and all events are discarded. The overflow flag tells the client that
// individual change tracking was lost and a full directory rescan is needed.
//
// Thread Safety:
// NotificationQueue is safe for concurrent use by multiple goroutines.
type NotificationQueue struct {
	mu       sync.Mutex
	events   []DirNotification
	capacity int
	overflow bool
	flushCh  chan struct{}
}

// NewNotificationQueue creates a new bounded notification queue.
//
// Parameters:
//   - capacity: Maximum number of events before overflow collapse.
//     Use DefaultNotificationQueueCapacity (1024) for the default.
func NewNotificationQueue(capacity int) *NotificationQueue {
	if capacity <= 0 {
		capacity = DefaultNotificationQueueCapacity
	}
	return &NotificationQueue{
		events:   make([]DirNotification, 0, capacity),
		capacity: capacity,
		flushCh:  make(chan struct{}, 1),
	}
}

// Push adds a directory change notification to the queue.
//
// If the queue is at capacity, the overflow flag is set and all events
// are discarded (collapsed). The Drain method will report the overflow.
func (q *NotificationQueue) Push(notification DirNotification) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// If already overflowed, discard silently
	if q.overflow {
		return
	}

	if len(q.events) >= q.capacity {
		// Overflow: discard all events and set flag
		q.events = q.events[:0]
		q.overflow = true
		// Signal so consumers waiting on FlushCh detect overflow via Drain()
		select {
		case q.flushCh <- struct{}{}:
		default:
		}
		return
	}

	q.events = append(q.events, notification)

	// Signal flush channel at threshold (capped at capacity for small queues)
	threshold := notificationFlushThreshold
	if q.capacity < threshold {
		threshold = q.capacity
	}
	if len(q.events) == threshold {
		select {
		case q.flushCh <- struct{}{}:
		default:
			// Channel already has a pending signal
		}
	}
}

// Drain returns all queued events and the overflow flag, then resets the queue.
//
// If overflow is true, the returned events slice is empty and the caller
// should perform a full directory rescan instead of processing individual events.
//
// Returns:
//   - events: The queued notifications (empty if overflow occurred)
//   - overflow: True if the queue overflowed and events were lost
func (q *NotificationQueue) Drain() ([]DirNotification, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	events := make([]DirNotification, len(q.events))
	copy(events, q.events)
	overflow := q.overflow

	// Reset queue state and drain flush channel so subsequent threshold
	// crossings will re-signal correctly.
	q.events = q.events[:0]
	q.overflow = false
	select {
	case <-q.flushCh:
	default:
	}

	return events, overflow
}

// Len returns the current number of queued events.
func (q *NotificationQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

// FlushCh returns a channel that is signaled when the queue length first
// reaches the flush threshold (100 events). This allows consumers to
// proactively drain the queue before it overflows.
//
// The channel is buffered with capacity 1, so at most one signal is pending.
// The signal fires exactly once per crossing from below to exactly the
// threshold. After a Drain() resets the count to zero, the next batch of
// pushes will signal again when the threshold is reached.
func (q *NotificationQueue) FlushCh() <-chan struct{} {
	return q.flushCh
}
