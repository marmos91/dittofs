package postgres

import "time"

// PoolConnectionAcquireTimeout exposes the checkout bound to the package's
// external tests, so a test asserting on it reads the real value instead of
// mirroring a copy that can drift. Test-only: a _test.go file is compiled into
// the test binary and excluded from every non-test build, so nothing ships.
const PoolConnectionAcquireTimeout time.Duration = poolConnectionAcquireTimeout
