package lock

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// NotificationQueue Basic Tests
// ============================================================================

func TestNotificationQueue_NewNotificationQueue(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(1024)
	require.NotNil(t, q)
	assert.Equal(t, 0, q.Len())
}

func TestNotificationQueue_PushAndDrain(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(1024)

	q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "file1.txt"})
	q.Push(DirNotification{ChangeType: DirChangeRemoveEntry, EntryName: "file2.txt"})

	assert.Equal(t, 2, q.Len())

	events, overflow := q.Drain()
	assert.False(t, overflow)
	assert.Len(t, events, 2)
	assert.Equal(t, "file1.txt", events[0].EntryName)
	assert.Equal(t, "file2.txt", events[1].EntryName)

	// After drain, queue should be empty
	assert.Equal(t, 0, q.Len())
	events2, overflow2 := q.Drain()
	assert.False(t, overflow2)
	assert.Len(t, events2, 0)
}

func TestNotificationQueue_OverflowCollapse(t *testing.T) {
	t.Parallel()

	capacity := 5
	q := NewNotificationQueue(capacity)

	// Fill to capacity
	for i := 0; i < capacity; i++ {
		q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "file"})
	}
	assert.Equal(t, capacity, q.Len())

	// Push one more - should trigger overflow
	q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "extra"})

	events, overflow := q.Drain()
	assert.True(t, overflow, "should report overflow")
	// Overflow collapses to a single "rescan needed" event
	assert.Len(t, events, 0, "overflow should collapse all events")
}

func TestNotificationQueue_Len(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(100)
	assert.Equal(t, 0, q.Len())

	q.Push(DirNotification{ChangeType: DirChangeAddEntry})
	assert.Equal(t, 1, q.Len())

	q.Push(DirNotification{ChangeType: DirChangeRemoveEntry})
	assert.Equal(t, 2, q.Len())
}

// ============================================================================
// NotificationQueue Flush Channel Tests
// ============================================================================

func TestNotificationQueue_FlushCh_Signaled(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(1024)

	ch := q.FlushCh()
	require.NotNil(t, ch)

	// Push 100 events to reach threshold
	for i := 0; i < 100; i++ {
		q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "file"})
	}

	// Flush channel should be signaled
	select {
	case <-ch:
		// Good - signaled
	default:
		t.Error("FlushCh should have been signaled after 100 events")
	}
}

func TestNotificationQueue_FlushCh_NotSignaledBelowThreshold(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(1024)

	ch := q.FlushCh()

	// Push fewer than 100 events
	for i := 0; i < 50; i++ {
		q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "file"})
	}

	// Flush channel should NOT be signaled
	select {
	case <-ch:
		t.Error("FlushCh should not be signaled below threshold")
	default:
		// Good - not signaled
	}
}

// ============================================================================
// NotificationQueue Thread Safety Tests
// ============================================================================

func TestNotificationQueue_ConcurrentPush(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(10000)

	var wg sync.WaitGroup
	pushCount := 100
	goroutineCount := 10

	for g := 0; g < goroutineCount; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < pushCount; i++ {
				q.Push(DirNotification{ChangeType: DirChangeAddEntry, EntryName: "file"})
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, pushCount*goroutineCount, q.Len())
}

func TestNotificationQueue_ConcurrentPushAndDrain(t *testing.T) {
	t.Parallel()

	q := NewNotificationQueue(10000)

	var wg sync.WaitGroup

	// Producers
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				q.Push(DirNotification{ChangeType: DirChangeAddEntry})
			}
		}()
	}

	// Consumers
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				q.Drain()
			}
		}()
	}

	wg.Wait()
	// Just verifying no panics or data races
}

// ============================================================================
// DirNotification Tests
// ============================================================================

func TestDirNotification_Fields(t *testing.T) {
	t.Parallel()

	n := DirNotification{
		ChangeType: DirChangeRenameEntry,
		EntryName:  "current_name",
		OldName:    "old_name",
		NewName:    "new_name",
	}

	assert.Equal(t, DirChangeRenameEntry, n.ChangeType)
	assert.Equal(t, "current_name", n.EntryName)
	assert.Equal(t, "old_name", n.OldName)
	assert.Equal(t, "new_name", n.NewName)
}
