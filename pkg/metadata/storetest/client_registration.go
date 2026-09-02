package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ClientRegistrationStoreProvider is implemented by metadata stores that
// expose a ClientRegistrationStore. The suite type-asserts the store to this
// and skips if unimplemented.
type ClientRegistrationStoreProvider interface {
	ClientRegistrationStore() lock.ClientRegistrationStore
}

// registrationTimeTolerance bounds RegisteredAt round-trip drift, the way the
// recovery suite does: postgres keeps TIMESTAMPTZ at microsecond resolution.
const registrationTimeTolerance = time.Millisecond

func testRegistration(clientID, monName string, priv byte) *lock.PersistedClientRegistration {
	reg := &lock.PersistedClientRegistration{
		ClientID:     clientID,
		MonName:      monName,
		CallbackHost: "callback." + clientID,
		CallbackProg: 100021,
		CallbackVers: 4,
		CallbackProc: 23,
		RegisteredAt: time.Now().UTC().Truncate(time.Microsecond),
		ServerEpoch:  7,
	}
	// A distinct, fully-populated Priv: the column is variable-length and the
	// field is a fixed 16 bytes, so a backend that truncates or drops it reads
	// as correct against a zero value.
	for i := range reg.Priv {
		reg.Priv[i] = priv + byte(i)
	}
	return reg
}

func requireRegistrationEqual(t *testing.T, want, got *lock.PersistedClientRegistration) {
	t.Helper()
	require.NotNil(t, got)
	require.Equal(t, want.ClientID, got.ClientID)
	require.Equal(t, want.MonName, got.MonName)
	require.Equal(t, want.Priv, got.Priv, "the 16-byte Priv blob did not survive the round trip")
	require.Equal(t, want.CallbackHost, got.CallbackHost)
	require.Equal(t, want.CallbackProg, got.CallbackProg)
	require.Equal(t, want.CallbackVers, got.CallbackVers)
	require.Equal(t, want.CallbackProc, got.CallbackProc)
	require.Equal(t, want.ServerEpoch, got.ServerEpoch)
	require.WithinDuration(t, want.RegisteredAt, got.RegisteredAt, registrationTimeTolerance)
}

// RunClientRegistrationStoreTests runs the cross-backend conformance suite for
// ClientRegistrationStore.
//
// This is the store the NSM crash-recovery path reads on startup to decide who
// gets an SM_NOTIFY, so a registration that fails to persist costs a client its
// chance to reclaim its locks — silently, since nothing on the write path
// looks at the row again.
func RunClientRegistrationStoreTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	if _, ok := factory(t).(ClientRegistrationStoreProvider); !ok {
		t.Skip("store does not implement ClientRegistrationStoreProvider")
		return
	}

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		want := testRegistration("client-a", "mon-a", 0x10)
		require.NoError(t, s.PutClientRegistration(ctx, want))

		got, err := s.GetClientRegistration(ctx, "client-a")
		require.NoError(t, err)
		requireRegistrationEqual(t, want, got)
	})

	t.Run("GetMissingIsNilNotError", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()

		got, err := s.GetClientRegistration(t.Context(), "never-registered")
		require.NoError(t, err, "an absent registration is not an error")
		require.Nil(t, got)
	})

	t.Run("PutReplacesExisting", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-a", "mon-old", 0x10)))

		updated := testRegistration("client-a", "mon-new", 0x20)
		require.NoError(t, s.PutClientRegistration(ctx, updated))

		got, err := s.GetClientRegistration(ctx, "client-a")
		require.NoError(t, err)
		requireRegistrationEqual(t, updated, got)

		all, err := s.ListClientRegistrations(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1, "a re-registration must replace the row, not add one")
	})

	t.Run("ListReturnsAll", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-a", "mon-a", 0x10)))
		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-b", "mon-b", 0x20)))

		all, err := s.ListClientRegistrations(ctx)
		require.NoError(t, err)
		require.Len(t, all, 2)

		byID := map[string]*lock.PersistedClientRegistration{}
		for _, reg := range all {
			byID[reg.ClientID] = reg
		}
		require.Contains(t, byID, "client-a")
		require.Contains(t, byID, "client-b")
		require.Equal(t, byte(0x20), byID["client-b"].Priv[0],
			"a listed row lost its Priv blob, which the single-row read kept")
	})

	t.Run("Delete", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-a", "mon-a", 0x10)))
		require.NoError(t, s.DeleteClientRegistration(ctx, "client-a"))

		got, err := s.GetClientRegistration(ctx, "client-a")
		require.NoError(t, err)
		require.Nil(t, got)

		require.NoError(t, s.DeleteClientRegistration(ctx, "client-a"),
			"deleting an absent registration is not an error")
	})

	t.Run("DeleteAllReportsCount", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-a", "mon-a", 0x10)))
		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-b", "mon-b", 0x20)))

		n, err := s.DeleteAllClientRegistrations(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, n, "the count is what SM_UNMON_ALL reports back to the caller")

		all, err := s.ListClientRegistrations(ctx)
		require.NoError(t, err)
		require.Empty(t, all)
	})

	t.Run("DeleteByMonNameTakesOnlyThatHost", func(t *testing.T) {
		s := factory(t).(ClientRegistrationStoreProvider).ClientRegistrationStore()
		ctx := t.Context()

		// Two clients monitoring the crashed host, one monitoring another. A
		// delete that ignored its predicate would report 3 and leave nothing.
		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-a", "crashed-host", 0x10)))
		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-b", "crashed-host", 0x20)))
		require.NoError(t, s.PutClientRegistration(ctx, testRegistration("client-c", "healthy-host", 0x30)))

		n, err := s.DeleteClientRegistrationsByMonName(ctx, "crashed-host")
		require.NoError(t, err)
		require.Equal(t, 2, n)

		all, err := s.ListClientRegistrations(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, "client-c", all[0].ClientID)
	})
}
