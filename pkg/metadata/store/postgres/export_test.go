package postgres

import "time"

// PoolConnectionAcquireTimeout exposes the checkout bound to the package's
// external tests, so a test asserting on it reads the real value instead of
// mirroring a copy that can drift. Test-only: the file is a _test.go, so it is
// never compiled into the binary.
const PoolConnectionAcquireTimeout time.Duration = poolConnectionAcquireTimeout
