// Package grantfsm is the caching grant's state machine: which kinds are
// valid, how a recall narrows a grant, stage by stage, how an acknowledgement
// and a timeout end one, how a later open widens one, and the epoch that
// orders its changes. It is a pure leaf of open state.
package grantfsm

import "errors"

// Kind is what a grant lets its holder cache: reads, writes, and handles
// kept open after the application closed them, in any valid combination.
type Kind uint8

const (
	Read Kind = 1 << iota
	Write
	Handle

	None Kind = 0
)

// ErrNotHeld is an acknowledgement naming caching the grant does not hold,
// or that its recall did not offer.
var ErrNotHeld = errors.New("grantfsm: acknowledgement names caching not held")

// Valid reports whether k is a kind a grant may hold: none, or read with any
// of write and handle. Write or handle caching without read is not.
func Valid(k Kind) bool { return k == None || k&Read != 0 && k&^(Read|Write|Handle) == 0 }

// Within is the most of k that allowed leaves: k's caching in allowed, or
// none where that is not a valid kind.
func Within(k, allowed Kind) Kind {
	if w := k & allowed; Valid(w) {
		return w
	}
	return None
}

// Compatible reports whether two holders' grants of kinds a and b may be held
// on one file together: write caching is held alone, and read caching beside
// any other read caching.
func Compatible(a, b Kind) bool {
	return (a&Write == 0 || b&(Read|Write) == 0) && (b&Write == 0 || a&(Read|Write) == 0)
}

// Grant is one grant's state.
type Grant struct {
	Kind   Kind
	Epoch  uint16  // raised by each change of Kind it announces; zero: not counted
	Recall *Recall // the recall in flight; nil: none
}

// Recall is a recall in flight.
type Recall struct {
	To       Kind // the kind this stage asks the holder to go to
	Required Kind // the kind every conflict met since it began leaves: To, or less
}

// New is a grant of kind k, its epoch counted from 1 if counted.
func New(k Kind, counted bool) Grant {
	g := Grant{Kind: k}
	if counted {
		g.Epoch = 1
	}
	return g
}

// raise raises a counted epoch, never to zero, which means not counted.
func (g *Grant) raise() {
	if g.Epoch == 0 {
		return
	}
	if g.Epoch++; g.Epoch == 0 {
		g.Epoch = 1
	}
}

// Break narrows g to what allowed leaves, and reports whether that starts a
// recall: with none in flight, one to Within(Kind, allowed), raising the
// epoch. With one in flight, it starts none and narrows the target the
// recall must reach, which later stages ask for.
func (g *Grant) Break(allowed Kind) bool {
	if g.Recall != nil {
		g.Recall.Required = Within(g.Recall.Required, allowed)
		return false
	}
	to := Within(g.Kind, allowed)
	if to == g.Kind {
		return false
	}
	g.Recall = &Recall{To: to, Required: to}
	g.raise()
	return true
}

// Ack applies the holder's acknowledgement to kind to, and reports whether
// it starts the next stage of the recall.
//
// One with a recall in flight goes to at most what the recall offered, so an
// answer to an earlier stage, asking more than this one offers, is refused;
// if that still holds caching beyond what the recall must reach, the next
// stage asks for that target, keeping read caching while it gives up write or
// handle caching, and raises the epoch. One with none in flight is the holder
// returning or narrowing the grant unasked, to at most what it holds.
func (g *Grant) Ack(to Kind) (next bool, err error) {
	if !Valid(to) {
		return false, ErrNotHeld
	}
	if g.Recall == nil {
		if to&^g.Kind != 0 {
			return false, ErrNotHeld
		}
		g.Kind = to
		return false, nil
	}
	if to&^g.Recall.To != 0 {
		return false, ErrNotHeld
	}
	g.Kind = to
	if to&^g.Recall.Required == 0 {
		g.Recall = nil
		return false, nil
	}
	stage := g.Recall.Required
	if to&(Write|Handle) != 0 {
		stage |= Read
	}
	g.Recall.To = stage
	g.raise()
	return true, nil
}

// Timeout ends the recall in flight unanswered: the grant is cut to none.
//
// decision: the cut raises no epoch. The epoch numbers the changes sent to
// the holder, and a cut is sent nothing; no acknowledgement carries an epoch,
// so none is judged by it either. Raise it here if a protocol is served that
// announces a timed-out break.
func (g *Grant) Timeout() {
	if g.Recall == nil {
		return
	}
	g.Kind, g.Recall = None, nil
}

// Upgrade widens g to want, on a later open under its key, if want is a valid
// kind holding all g holds and more, within allowed, and no recall is in
// flight: it never narrows, and never breaks another holder. It reports
// whether g changed, raising the epoch if it did.
func (g *Grant) Upgrade(want, allowed Kind) bool {
	if g.Recall != nil || !Valid(want) || want == g.Kind || want&g.Kind != g.Kind || want&^allowed != 0 {
		return false
	}
	g.Kind = want
	g.raise()
	return true
}
