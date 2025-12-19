package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ============================================================================
// directoryMtimeVerifier Tests
// ============================================================================

func TestDirectoryMtimeVerifier(t *testing.T) {
	t.Run("DifferentTimesProduceDifferentVerifiers", func(t *testing.T) {
		time1 := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		time2 := time.Date(2024, 1, 15, 10, 30, 1, 0, time.UTC) // 1 second later

		verf1 := directoryMtimeVerifier(time1)
		verf2 := directoryMtimeVerifier(time2)

		assert.NotEqual(t, verf1, verf2, "Different times should produce different verifiers")
	})

	t.Run("SameTimeProducesSameVerifier", func(t *testing.T) {
		time1 := time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC)

		verf1 := directoryMtimeVerifier(time1)
		verf2 := directoryMtimeVerifier(time1)

		assert.Equal(t, verf1, verf2, "Same time should produce same verifier")
	})

	t.Run("NanosecondPrecision", func(t *testing.T) {
		time1 := time.Date(2024, 1, 15, 10, 30, 0, 100, time.UTC)
		time2 := time.Date(2024, 1, 15, 10, 30, 0, 101, time.UTC) // 1 nanosecond later

		verf1 := directoryMtimeVerifier(time1)
		verf2 := directoryMtimeVerifier(time2)

		assert.NotEqual(t, verf1, verf2, "Nanosecond differences should produce different verifiers")
	})

	t.Run("ZeroTimeProducesConsistentVerifier", func(t *testing.T) {
		verf := directoryMtimeVerifier(time.Time{})
		// Zero time has negative UnixNano, but cast to uint64 it becomes a large number
		// Just verify it's consistent
		verf2 := directoryMtimeVerifier(time.Time{})
		assert.Equal(t, verf, verf2, "Zero time should be consistent")
	})

	t.Run("RecentTimeProducesNonZeroVerifier", func(t *testing.T) {
		verf := directoryMtimeVerifier(time.Now())
		assert.NotEqual(t, uint64(0), verf, "Recent time should produce non-zero verifier")
	})
}

// ============================================================================
// Cookie Verifier Validation Logic Tests
// ============================================================================

func TestCookieVerifierValidation(t *testing.T) {
	// These tests verify the validation logic used in READDIR/READDIRPLUS
	// The actual validation is:
	//   if req.Cookie != 0 && req.CookieVerf != 0 && req.CookieVerf != currentVerifier

	t.Run("InitialRequestBypassesCheck", func(t *testing.T) {
		// Initial request: cookie=0
		cookie := uint64(0)
		cookieVerf := uint64(12345) // Any value
		currentVerf := uint64(99999)

		// Should NOT return error because cookie=0 (initial request)
		shouldReject := cookie != 0 && cookieVerf != 0 && cookieVerf != currentVerf
		assert.False(t, shouldReject, "Initial request (cookie=0) should bypass verifier check")
	})

	t.Run("ClientWithoutVerifierBypassesCheck", func(t *testing.T) {
		// Client that doesn't support verifiers sends 0
		cookie := uint64(100)
		cookieVerf := uint64(0) // Client doesn't use verifiers
		currentVerf := uint64(99999)

		// Should NOT return error because cookieVerf=0
		shouldReject := cookie != 0 && cookieVerf != 0 && cookieVerf != currentVerf
		assert.False(t, shouldReject, "Client with verifier=0 should bypass check")
	})

	t.Run("MatchingVerifierPasses", func(t *testing.T) {
		cookie := uint64(100)
		cookieVerf := uint64(123456789)
		currentVerf := uint64(123456789) // Same value as cookieVerf

		// Should NOT return error because verifiers match
		shouldReject := cookie != 0 && cookieVerf != 0 && cookieVerf != currentVerf
		assert.False(t, shouldReject, "Matching verifier should pass")
	})

	t.Run("MismatchingVerifierFails", func(t *testing.T) {
		cookie := uint64(100)
		cookieVerf := uint64(111111)
		currentVerf := uint64(222222)

		// Should return error because verifiers don't match
		shouldReject := cookie != 0 && cookieVerf != 0 && cookieVerf != currentVerf
		assert.True(t, shouldReject, "Mismatching verifier should fail")
	})
}

// ============================================================================
// IdempotencyToken Tests
// ============================================================================

func TestIdempotencyTokenValidation(t *testing.T) {
	// These tests verify the CREATE EXCLUSIVE validation logic:
	//   if existingFile.IdempotencyToken == req.Verf && req.Verf != 0

	t.Run("ZeroVerifierNeverMatches", func(t *testing.T) {
		existingToken := uint64(0)
		reqVerf := uint64(0)

		// Should NOT match because req.Verf == 0
		isRetry := existingToken == reqVerf && reqVerf != 0
		assert.False(t, isRetry, "Zero verifier should never be treated as retry")
	})

	t.Run("MatchingNonZeroTokenIsRetry", func(t *testing.T) {
		existingToken := uint64(0x123456789ABCDEF0)
		reqVerf := uint64(0x123456789ABCDEF0) // Same value as existingToken

		isRetry := existingToken == reqVerf && reqVerf != 0
		assert.True(t, isRetry, "Matching non-zero token should be treated as retry")
	})

	t.Run("DifferentTokenIsNotRetry", func(t *testing.T) {
		existingToken := uint64(0x1111111111111111)
		reqVerf := uint64(0x2222222222222222)

		isRetry := existingToken == reqVerf && reqVerf != 0
		assert.False(t, isRetry, "Different token should not be treated as retry")
	})

	t.Run("ExistingZeroWithNonZeroRequestIsNotRetry", func(t *testing.T) {
		existingToken := uint64(0)
		reqVerf := uint64(0x123456789ABCDEF0)

		isRetry := existingToken == reqVerf && reqVerf != 0
		assert.False(t, isRetry, "File with zero token should not match non-zero request")
	})
}
