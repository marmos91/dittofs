package metadata

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func namespaceTestHandle(t *testing.T, n int) FileHandle {
	t.Helper()
	id := uuid.MustParse(fmt.Sprintf("aabbccdd-0000-0000-0000-%012x", n))
	h, err := EncodeShareHandle("/guard", id)
	require.NoError(t, err)
	return h
}

func TestNamespaceGuardCanonicalCollisionAndPromotion(t *testing.T) {
	svc := New()
	a := namespaceTestHandle(t, 1)
	index, err := namespaceShard(a)
	require.NoError(t, err)
	alias := FileHandle("/guard:" + strings.ToUpper(strings.SplitN(string(a), ":", 2)[1]))
	aliasIndex, err := namespaceShard(alias)
	require.NoError(t, err)
	require.Equal(t, index, aliasIndex)

	var collision FileHandle
	for n := 2; n < 10000; n++ {
		candidate := namespaceTestHandle(t, n)
		candidateIndex, err := namespaceShard(candidate)
		require.NoError(t, err)
		if candidateIndex == index {
			collision = candidate
			break
		}
	}
	require.NotNil(t, collision)
	guard, err := svc.lockNamespace(namespaceAccess{handle: a}, namespaceAccess{handle: alias}, namespaceAccess{handle: collision, exclusive: true})
	require.NoError(t, err)
	require.Equal(t, 1, guard.count)
	require.False(t, svc.namespaceShards[index].TryRLock(), "a colliding writer must promote the one acquired slot")
	guard.unlock()
	guard.unlock()
	require.True(t, svc.namespaceShards[index].TryLock())
	svc.namespaceShards[index].Unlock()

	_, err = svc.lockNamespace(namespaceAccess{handle: a}, namespaceAccess{handle: FileHandle("invalid")})
	require.Error(t, err)
	require.True(t, svc.namespaceShards[index].TryLock(), "invalid input must not retain an earlier slot")
	svc.namespaceShards[index].Unlock()
}

func TestNamespaceGuardSharedAndReverseOrder(t *testing.T) {
	svc := New()
	a, b := namespaceTestHandle(t, 1), namespaceTestHandle(t, 2)
	first, err := svc.lockNamespace(namespaceAccess{handle: a})
	require.NoError(t, err)
	index, err := namespaceShard(a)
	require.NoError(t, err)
	require.True(t, svc.namespaceShards[index].TryRLock(), "same-directory child mutations must retain shared admission")
	svc.namespaceShards[index].RUnlock()
	first.unlock()

	start := make(chan struct{})
	done := make(chan error, 2)
	for _, handles := range [][2]FileHandle{{a, b}, {b, a}} {
		go func() {
			<-start
			for range 200 {
				guard, err := svc.lockNamespace(namespaceAccess{handle: handles[0]}, namespaceAccess{handle: handles[1], exclusive: true})
				if err != nil {
					done <- err
					return
				}
				guard.unlock()
			}
			done <- nil
		}()
	}
	close(start)
	for range 2 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("reversed namespace requests deadlocked")
		}
	}
}
