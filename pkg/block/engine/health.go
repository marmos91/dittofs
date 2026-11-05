package engine

import (
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// conditionJournalLost is RFC 25 §3.9's condition for a journal found
// missing or foreign: unhealthy, until an operator acknowledges the loss.
const conditionJournalLost = "journal_lost"

// Condition is one health condition the engine raises, in the fields of
// RFC 25 §8.2's, so the composition root adapts it into the health evaluator.
type Condition struct {
	Name    string
	Subject Subject
	Cause   error  // the error or reason that raised it
	Detail  string // a constant message and the figures that matter
}

// Subject is what a condition is about (RFC 25 §8.2).
type Subject struct {
	Scope Scope
	ID    string
	Name  string
}

// Scope is the kind of thing a condition is about (RFC 25 §3.3).
type Scope uint8

const (
	ScopeJournal Scope = iota + 1 // a journal device: ID its identity, Name its directory
)

// JournalStats is one device's journal, as Stats reports it.
type JournalStats struct {
	Conditions []Condition
}

// ShareStats is one share, as Stats reports it. Device and Refused are set
// for a share refused at start: its device, and why.
type ShareStats struct {
	Conditions []Condition
	Device     string
	Refused    error
}

// NodeStats is the node's own conditions.
type NodeStats struct {
	Conditions []Condition
}

// newStats is an empty Stats, its maps made.
func newStats() Stats {
	return Stats{
		Journals: map[string]JournalStats{},
		Shares:   map[entity.ShareID]ShareStats{},
	}
}

// clone is a copy of s that shares no map or slice with it, so a caller
// keeps what Stats returns.
func (s Stats) clone() Stats {
	out := newStats()

	for dev, j := range s.Journals {
		out.Journals[dev] = JournalStats{
			Conditions: slices.Clone(j.Conditions),
		}
	}

	for id, sh := range s.Shares {
		sh.Conditions = slices.Clone(sh.Conditions)
		out.Shares[id] = sh
	}

	out.Node.Conditions = slices.Clone(s.Node.Conditions)

	return out
}
