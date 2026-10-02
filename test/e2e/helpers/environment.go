//go:build e2e

// Package helpers provides test environment and scope management for E2E tests.
// It wraps the existing framework package to provide container management and
// per-test isolation through unique S3 prefixes.
package helpers

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestEnvironment wraps the existing framework helpers for container management.
// It uses the shared singleton pattern from framework/containers.go to ensure
// containers are reused within a single test run.
//
// Container lifecycle:
// - Containers are started lazily when first needed (by NewTestEnvironment)
// - Containers are shared across all tests in a single `go test` run
// - Containers are terminated by Cleanup() called from TestMain
type TestEnvironment struct {
	ctx      context.Context
	lsHelper *framework.LocalstackHelper
}

// Global environment instance for TestMain integration
var globalEnv *TestEnvironment

// NewTestEnvironment creates a new TestEnvironment using framework helpers.
// It starts or reuses the shared Localstack container.
// Failures are reported via t.Fatal for fail-fast behavior.
//
// It is idempotent - calling it multiple times returns environments
// that share the same underlying containers via framework singletons.
func NewTestEnvironment(t *testing.T) *TestEnvironment {
	t.Helper()

	ctx := context.Background()

	// Start or reuse shared Localstack container via framework singleton
	lsHelper := framework.NewLocalstackHelper(t)

	env := &TestEnvironment{
		ctx:      ctx,
		lsHelper: lsHelper,
	}

	// Update global reference for TestMain cleanup
	globalEnv = env

	return env
}

// NewTestEnvironmentForMain creates a placeholder TestEnvironment for TestMain.
// Unlike NewTestEnvironment, this doesn't start containers immediately.
// Containers are started lazily when individual tests call NewTestEnvironment(t).
//
// This design is necessary because:
// 1. framework.NewLocalstackHelper requires *testing.T
// 2. TestMain doesn't have a *testing.T
// 3. The framework already uses singleton pattern for container reuse
//
// The returned environment is used only for cleanup coordination.
func NewTestEnvironmentForMain(ctx context.Context) *TestEnvironment {
	env := &TestEnvironment{
		ctx: ctx,
		// lsHelper is nil - populated by the first NewTestEnvironment call
	}
	globalEnv = env
	return env
}

// Cleanup terminates all shared containers.
// This should be called from TestMain after all tests complete.
func (env *TestEnvironment) Cleanup() {
	framework.CleanupSharedContainers()
}

// NewScope creates a new TestScope for per-test isolation.
// Each scope gets a unique S3 prefix.
func (env *TestEnvironment) NewScope(t *testing.T) *TestScope {
	t.Helper()
	return newScope(t, env)
}

// LocalstackHelper returns the underlying Localstack helper.
func (env *TestEnvironment) LocalstackHelper() *framework.LocalstackHelper {
	return env.lsHelper
}

// S3Client returns the S3 client from the Localstack helper.
func (env *TestEnvironment) S3Client() *s3.Client {
	if env.lsHelper == nil {
		return nil
	}
	return env.lsHelper.Client
}

// S3Endpoint returns the Localstack endpoint URL.
func (env *TestEnvironment) S3Endpoint() string {
	if env.lsHelper == nil {
		return ""
	}
	return env.lsHelper.Endpoint
}

// Context returns the environment's context.
func (env *TestEnvironment) Context() context.Context {
	return env.ctx
}
