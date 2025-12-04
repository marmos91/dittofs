package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Enabled)
	assert.Equal(t, "dittofs", cfg.ServiceName)
	assert.Equal(t, "dev", cfg.ServiceVersion)
	assert.Equal(t, "localhost:4317", cfg.Endpoint)
	assert.True(t, cfg.Insecure)
	assert.Equal(t, 1.0, cfg.SampleRate)
}

func TestInitDisabled(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled = false

	shutdown, err := Init(ctx, cfg)
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	// Should be able to call shutdown without error
	err = shutdown(ctx)
	assert.NoError(t, err)

	// Should not be enabled
	assert.False(t, IsEnabled())
}

func TestTracerReturnsNoOp(t *testing.T) {
	// Reset state
	tracer = nil
	enabled = false

	// Without initialization, should return no-op tracer
	tr := Tracer()
	require.NotNil(t, tr)
}

func TestStartSpan(t *testing.T) {
	ctx := context.Background()

	// Even without initialization, StartSpan should work (no-op)
	newCtx, span := StartSpan(ctx, "test.operation")
	require.NotNil(t, newCtx)
	require.NotNil(t, span)

	// Should be able to end the span
	span.End()
}

func TestSpanFromContext(t *testing.T) {
	ctx := context.Background()

	// Should return a span even without active span
	span := SpanFromContext(ctx)
	require.NotNil(t, span)
}

func TestAddEvent(t *testing.T) {
	ctx := context.Background()

	// Should not panic with no active span
	require.NotPanics(t, func() {
		AddEvent(ctx, "test.event")
	})
}

func TestRecordError(t *testing.T) {
	ctx := context.Background()

	// Should not panic with nil error
	require.NotPanics(t, func() {
		RecordError(ctx, nil)
	})

	// Should not panic with error
	require.NotPanics(t, func() {
		RecordError(ctx, errors.New("test error"))
	})
}

func TestSetStatus(t *testing.T) {
	ctx := context.Background()

	// Should not panic
	require.NotPanics(t, func() {
		SetStatus(ctx, codes.Ok, "success")
	})

	require.NotPanics(t, func() {
		SetStatus(ctx, codes.Error, "failed")
	})
}

func TestSetAttributes(t *testing.T) {
	ctx := context.Background()

	// Should not panic
	require.NotPanics(t, func() {
		SetAttributes(ctx, ClientIP("192.168.1.1"))
	})
}

func TestTraceID(t *testing.T) {
	ctx := context.Background()

	// Without active span, should return empty string
	traceID := TraceID(ctx)
	assert.Equal(t, "", traceID)
}

func TestSpanID(t *testing.T) {
	ctx := context.Background()

	// Without active span, should return empty string
	spanID := SpanID(ctx)
	assert.Equal(t, "", spanID)
}

func TestAttributeHelpers(t *testing.T) {
	t.Run("ClientIP", func(t *testing.T) {
		attr := ClientIP("192.168.1.100")
		assert.Equal(t, AttrClientIP, string(attr.Key))
		assert.Equal(t, "192.168.1.100", attr.Value.AsString())
	})

	t.Run("ClientAddr", func(t *testing.T) {
		attr := ClientAddr("192.168.1.100:12345")
		assert.Equal(t, AttrClientAddr, string(attr.Key))
		assert.Equal(t, "192.168.1.100:12345", attr.Value.AsString())
	})

	t.Run("RPCXID", func(t *testing.T) {
		attr := RPCXID(0x12345678)
		assert.Equal(t, AttrRPCXID, string(attr.Key))
		assert.Equal(t, int64(0x12345678), attr.Value.AsInt64())
	})

	t.Run("NFSProcedure", func(t *testing.T) {
		attr := NFSProcedure("READ")
		assert.Equal(t, AttrNFSProcedure, string(attr.Key))
		assert.Equal(t, "READ", attr.Value.AsString())
	})

	t.Run("NFSHandle", func(t *testing.T) {
		attr := NFSHandle([]byte{0x01, 0x02, 0x03, 0x04})
		assert.Equal(t, AttrNFSHandle, string(attr.Key))
		assert.Equal(t, "01020304", attr.Value.AsString())
	})

	t.Run("NFSHandleHex", func(t *testing.T) {
		attr := NFSHandleHex("abcd1234")
		assert.Equal(t, AttrNFSHandle, string(attr.Key))
		assert.Equal(t, "abcd1234", attr.Value.AsString())
	})

	t.Run("NFSShare", func(t *testing.T) {
		attr := NFSShare("/export")
		assert.Equal(t, AttrNFSShare, string(attr.Key))
		assert.Equal(t, "/export", attr.Value.AsString())
	})

	t.Run("NFSOffset", func(t *testing.T) {
		attr := NFSOffset(1024)
		assert.Equal(t, AttrNFSOffset, string(attr.Key))
		assert.Equal(t, int64(1024), attr.Value.AsInt64())
	})

	t.Run("NFSCount", func(t *testing.T) {
		attr := NFSCount(4096)
		assert.Equal(t, AttrNFSCount, string(attr.Key))
		assert.Equal(t, int64(4096), attr.Value.AsInt64())
	})

	t.Run("NFSSize", func(t *testing.T) {
		attr := NFSSize(1048576)
		assert.Equal(t, AttrNFSSize, string(attr.Key))
		assert.Equal(t, int64(1048576), attr.Value.AsInt64())
	})

	t.Run("NFSStatus", func(t *testing.T) {
		attr := NFSStatus(0)
		assert.Equal(t, AttrNFSStatus, string(attr.Key))
		assert.Equal(t, int64(0), attr.Value.AsInt64())
	})

	t.Run("NFSEOF", func(t *testing.T) {
		attr := NFSEOF(true)
		assert.Equal(t, AttrNFSEOF, string(attr.Key))
		assert.True(t, attr.Value.AsBool())
	})

	t.Run("UID", func(t *testing.T) {
		attr := UID(1000)
		assert.Equal(t, AttrUID, string(attr.Key))
		assert.Equal(t, int64(1000), attr.Value.AsInt64())
	})

	t.Run("GID", func(t *testing.T) {
		attr := GID(1000)
		assert.Equal(t, AttrGID, string(attr.Key))
		assert.Equal(t, int64(1000), attr.Value.AsInt64())
	})

	t.Run("CacheHit", func(t *testing.T) {
		attr := CacheHit(true)
		assert.Equal(t, AttrCacheHit, string(attr.Key))
		assert.True(t, attr.Value.AsBool())
	})

	t.Run("CacheSource", func(t *testing.T) {
		attr := CacheSource("dirty")
		assert.Equal(t, AttrCacheSource, string(attr.Key))
		assert.Equal(t, "dirty", attr.Value.AsString())
	})

	t.Run("ContentID", func(t *testing.T) {
		attr := ContentID("abc123")
		assert.Equal(t, AttrContentID, string(attr.Key))
		assert.Equal(t, "abc123", attr.Value.AsString())
	})

	t.Run("Bucket", func(t *testing.T) {
		attr := Bucket("my-bucket")
		assert.Equal(t, AttrBucket, string(attr.Key))
		assert.Equal(t, "my-bucket", attr.Value.AsString())
	})

	t.Run("StorageKey", func(t *testing.T) {
		attr := StorageKey("path/to/object")
		assert.Equal(t, AttrKey, string(attr.Key))
		assert.Equal(t, "path/to/object", attr.Value.AsString())
	})
}

func TestStartNFSSpan(t *testing.T) {
	ctx := context.Background()

	newCtx, span := StartNFSSpan(ctx, "READ", []byte{0x01, 0x02, 0x03, 0x04})
	require.NotNil(t, newCtx)
	require.NotNil(t, span)
	span.End()

	// With empty handle
	newCtx2, span2 := StartNFSSpan(ctx, "GETATTR", nil)
	require.NotNil(t, newCtx2)
	require.NotNil(t, span2)
	span2.End()

	// With additional attributes
	newCtx3, span3 := StartNFSSpan(ctx, "WRITE", []byte{0x01}, NFSOffset(0), NFSCount(4096))
	require.NotNil(t, newCtx3)
	require.NotNil(t, span3)
	span3.End()
}

func TestStartContentSpan(t *testing.T) {
	ctx := context.Background()

	newCtx, span := StartContentSpan(ctx, "read", "content-123")
	require.NotNil(t, newCtx)
	require.NotNil(t, span)
	span.End()

	// With additional attributes
	newCtx2, span2 := StartContentSpan(ctx, "write", "content-456", NFSOffset(0), NFSSize(1024))
	require.NotNil(t, newCtx2)
	require.NotNil(t, span2)
	span2.End()
}

func TestStartCacheSpan(t *testing.T) {
	ctx := context.Background()

	newCtx, span := StartCacheSpan(ctx, "lookup")
	require.NotNil(t, newCtx)
	require.NotNil(t, span)
	span.End()

	// With additional attributes
	newCtx2, span2 := StartCacheSpan(ctx, "write", CacheHit(false))
	require.NotNil(t, newCtx2)
	require.NotNil(t, span2)
	span2.End()
}
