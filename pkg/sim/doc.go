// Package sim holds what tests share to drive components from outside: the
// test clock a check advances and skews across components, and the fault
// interface every fault seam implements, which can fail, delay, drop, tear or
// reorder a named operation on a seeded schedule read from that clock.
//
// It is for tests only. Production builds nothing from it; the node's clock
// is in package clock.
package sim
