package apiclient

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlockStoreStats_IsEngineAlias asserts that the apiclient surface
// BlockStoreStats is the literal same type as engine.BlockStoreStats — not a
// look-alike struct. A type alias keeps the wire shape and lets values cross
// the apiclient/engine boundary without a conversion shim.
func TestBlockStoreStats_IsEngineAlias(t *testing.T) {
	var a BlockStoreStats
	var b engine.BlockStoreStats
	assert.Equal(t,
		reflect.TypeOf(a),
		reflect.TypeOf(b),
		"apiclient.BlockStoreStats must be a type alias of engine.BlockStoreStats",
	)
}

// TestBlockStoreStats_WireShapeRoundTrip captures the JSON wire shape so any
// future drift between server emission and client consumption is caught by
// the test suite. Field names + json tags are load-bearing for REST clients.
func TestBlockStoreStats_WireShapeRoundTrip(t *testing.T) {
	src := engine.BlockStoreStats{
		FileCount:           1,
		BlocksDirty:         2,
		BlocksLocal:         3,
		BlocksRemote:        4,
		BlocksTotal:         5,
		LocalDiskUsed:       6,
		LocalDiskMax:        7,
		LocalMemUsed:        8,
		LocalMemMax:         9,
		ReadBufferEntries:   10,
		ReadBufferUsed:      11,
		ReadBufferMax:       12,
		HasRemote:           true,
		PendingSyncs:        13,
		PendingUploads:      14,
		CompletedSyncs:      15,
		FailedSyncs:         16,
		RemoteHealthy:       true,
		EvictionSuspended:   true,
		OutageDurationSecs:  1.5,
		OfflineReadsBlocked: 17,
	}

	wire, err := json.Marshal(src)
	require.NoError(t, err)

	var dst BlockStoreStats
	require.NoError(t, json.Unmarshal(wire, &dst))

	assert.Equal(t, src, engine.BlockStoreStats(dst))
}
