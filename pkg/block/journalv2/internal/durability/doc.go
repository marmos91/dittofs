// Package durability makes appended records durable. It serves sync requests,
// groups concurrent ones into a single sync, enforces the share's sync policy
// and its bound, and handles a sync that failed: a later success never stands
// in for writes made before the failure, and a segment whose sync failed is
// not appended to again.
package durability
