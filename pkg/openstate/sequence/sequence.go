// Package sequence is the per-owner sequence slot: the number an owner's next
// numbered request must carry, and which answers advance it. It is a pure leaf
// of open state.
package sequence

import "github.com/marmos91/dittofs/pkg/metadata/entity"

// Kind is what an owner numbers its requests for.
type Kind uint8

const (
	Opens Kind = iota + 1 // opens, confirms, downgrades and closes
	Locks                 // locks and unlocks
)

// Owner names one owner: its client, its kind and the name the client gives
// it.
type Owner struct {
	Client entity.ClientID
	Kind   Kind
	Name   string
}

// Slot is an owner's sequence.
type Slot struct {
	Next      uint32 // the number its next request must carry
	Confirmed bool   // an owner of opens: set by its confirm, and until then only a confirm is accepted
}

// Outcome is how a numbered request was answered.
type Outcome uint8

const (
	Granted Outcome = iota + 1
	Refused         // refused with an answer that advances the sequence
	Exempt          // refused with an answer that advances nothing
)

// Book is the slots of the owners that number their requests. An owner with no
// slot is new: any number starts it.
type Book map[Owner]Slot

// Check reports whether o may send a request numbered n: the number its slot
// expects, and for an owner of opens not yet confirmed, only a confirm.
//
// decision: an unconfirmed owner's request other than its confirm is refused
// as out of sequence, which advances nothing, so its client starts over with
// a new owner. Treat it instead as a new owner replacing the unconfirmed one
// if a client is seen retrying under the same owner.
func (b Book) Check(o Owner, n uint32, confirming bool) bool {
	s, ok := b[o]
	return !ok || s.Next == n && (s.Confirmed || confirming)
}

// Answer records how o's request numbered n, which Check admitted, was
// answered. Only a granted request gives a new owner its slot, one past n, and
// a new owner of opens starts unconfirmed. Numbers wrap modulo 2^32.
//
// decision: a refused first request leaves its owner new, so the owner's next
// request is accepted whatever its number, which suits a client that counted
// the refusal and one that did not alike. Give the owner its slot on any
// answer that is not exempt if a client is seen relying on a stale number
// being refused after a refused first request.
func (b Book) Answer(o Owner, n uint32, a Outcome) {
	s, ok := b[o]
	switch {
	case a == Exempt:
	case !ok:
		if a == Granted {
			b[o] = Slot{Next: n + 1, Confirmed: o.Kind != Opens}
		}
	case s.Next == n:
		s.Next = n + 1
		b[o] = s
	}
}
