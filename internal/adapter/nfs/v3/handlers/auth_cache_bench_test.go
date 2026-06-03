package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
)

// BenchmarkBuildAuthContext_Uncached measures the per-RPC cost of rebuilding
// the auth context from scratch (share lookup + identity-store resolve +
// identity mapping) — the path taken before the 7 read-class v3 ops were
// switched to the shared auth cache.
func BenchmarkBuildAuthContext_Uncached(b *testing.B) {
	fx := handlertesting.NewHandlerFixture(b)
	ctx := fx.Context()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		authCtx, err := handlers.BuildAuthContextWithMapping(ctx, fx.Registry, ctx.Share)
		if err != nil {
			b.Fatal(err)
		}
		_ = authCtx
	}
}

// BenchmarkBuildAuthContext_Cached measures the same path served from the
// shared per-(share, flavor, credential) auth cache that LOOKUP / ACCESS /
// READDIR(PLUS) / READLINK / LINK / COMMIT now use.
func BenchmarkBuildAuthContext_Cached(b *testing.B) {
	fx := handlertesting.NewHandlerFixture(b)
	ctx := fx.Context()

	// Warm the cache once so the benchmark measures the hit path.
	if _, err := fx.Handler.GetCachedAuthContext(ctx); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		authCtx, err := fx.Handler.GetCachedAuthContext(ctx)
		if err != nil {
			b.Fatal(err)
		}
		_ = authCtx
	}
}
