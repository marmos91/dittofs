// Package sim declares the clock and fault interfaces shared by every
// component. A component reads time only through the clock, so a test can
// advance it and skew it across components. Every fault seam implements the
// fault interface: it can fail, delay, drop, tear or reorder a named
// operation, on a seeded schedule read from that clock.
package sim
