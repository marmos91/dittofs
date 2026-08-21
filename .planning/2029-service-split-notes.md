# #2029 — pkg/metadata/service.go god object: seam analysis

## Measured shape of the type (develop 70dc7778)

- `service.go` = 1583 lines, `Service` has **128 methods** across the package.
- **58** touch >=1 struct field; **70 are pure delegation** (resolve a store via
  `storeForHandle`, call it, map errors). Those 70 carry no state at all.
- Per-field use counts are tiny: `stores` 8, `lockManagers` 8, `storeCache` 4,
  `dirChangeNotifiers` 4, `removeGen` 4, `unifiedViews` 3, `quotas` 3,
  `writebackShares` 4, `graceCoordinator` 5, `graceDuration` 2,
  `byteRangeReleaseHook` 2, `durableExtent` 2, `createNameShards` 1.
- The only fields with real traffic are `pendingWrites` (23) and
  `identityQuotas` (19) — **both already behind their own named types.**

## The concern split has already been done, for every concern that separates

`CookieManager` (cookies.go), `PendingWritesTracker` (pending_writes.go),
`DirTimesTracker` (dir_times.go), `quotaLimits` (quota_limits.go) are already
extracted named types, each with its own file and its own mutex. These are the
four concerns that were separable, and they are separated.

## Seam A — `pkg/metadata/shares` sub-package (the runtime/ pattern): FAILS

Attempted and built. Hard import cycle:

    package pkg/metadata
      imports pkg/metadata/shares from service.go
      imports pkg/metadata from registry.go: import cycle not allowed

Structural reason, not an accident of naming: `pkg/controlplane/runtime` sits at
the TOP of the dependency stack, so `runtime/shares`, `runtime/stores` etc. can
depend *downward* on `pkg/metadata`. `runtime/shares` imports `pkg/metadata` and
never imports `runtime`. `pkg/metadata` is at the BOTTOM — it *defines* the
vocabulary (`Store`, `FileHandle`, `File`, `FileAttr`, `AuthContext`,
`UnifiedLockView`, `Permission`, `NewStaleHandleError`, `DecodeFileHandle`) that
any sub-package of it would have to import. There is no lower layer to put the
sub-services in. The runtime pattern does not transfer.

Rescuing it means moving the vocabulary types down into a `pkg/metadata/types`
package: **1130 references across 215 files** outside `pkg/metadata`. That is
not a behaviour-preserving move.

## Constraints any in-package split must still preserve

1. **register/remove TOCTOU.** `RegisterStoreForShare` snapshots
   `removeGen[share]` under `s.mu`, drops the lock for backend IO (epoch bump,
   `ListLocks`, replay, grace entry), then re-acquires and re-checks the
   generation *under the same acquisition that publishes* `stores` +
   `storeCache` + `lockManagers` + `dirChangeNotifiers`. Decision-to-publish and
   publish are one atomic step.
2. **Grace lockstep.** Exactly-once `OnLockGraceStart`/`OnLockGraceEnd` across
   four asymmetric paths, and `RemoveStoreForShare` must read `IsInGracePeriod`
   *before* `AbortGracePeriod` under the same `s.mu` that covers
   `lockManagers` + `graceCoordinator` + `removeGen`.
