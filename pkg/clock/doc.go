// Package clock is the node's one source of time. The composition root builds
// one clock and hands it to every component that reads time, so a check can
// replace it with a test clock and control time across all of them at once.
//
// The clock is written in standard-library types only. Each component declares
// the clock it reads in its own package, as the subset of these methods it
// uses, so this clock satisfies every such declaration as it is and no
// component imports this package to take it.
package clock
