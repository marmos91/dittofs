// Package conformance holds the journal's component end-to-end checks. Each
// check opens a journal on a temporary directory of a real filesystem, drives
// the public interface, and asserts on what comes back and on what is on disk.
package conformance
