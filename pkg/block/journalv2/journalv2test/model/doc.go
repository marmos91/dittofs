// Package model is a reference model of the journal as its caller sees it: per
// file, which bytes are held and which are absent, the content version and
// offloaded bit of each held byte, and the removal markers with their versions
// and settled state.
//
// The model keeps, per file, the records the caller's operations imply, and
// derives every answer by deciding each byte among them by precedence: the
// higher content version wins, and between equal versions the later record.
// That is the journal's own rule for rebuilding a file, so a crash is modelled
// by dropping the records it may have lost and deciding again.
//
// The model is driven with what the journal returned. Each operation takes the
// journal's result, checks that the RFC allows it, and applies it. Where the
// RFC leaves a choice to the journal, such as which stale fills a floor bucket
// refuses, how much of the dirty content one offer carries, or which unsynced
// records survive a crash, the model accepts any allowed outcome and continues
// from the one the journal took. After a crash, Compare finds the allowed state
// the journal shows and continues from it.
//
// The model is the expected answer only. It never stands in for storage.
//
// # Driving it
//
// Call the journal, then the model's operation of the same name with the
// journal's result: what the call returned, its error included, as the last
// arguments. For example, after v, err := h.WriteAt(id, off, p, mtime), call
// m.WriteAt(tag, id, off, p, mtime, v, err). Apply calls to the model in the
// order the journal's calls returned.
//
// An offload is three steps. Inside the Offload callback, first call Offer
// with the offer the journal gave; then call Report with each report and its
// error. After Offload returns, call EndOffer with what the callback returned
// and what Offload returned, whether or not the callback was called.
//
// A crash is Crash, the storage crash, then Reopen with the highest floor the
// journal was given, then Compare for every share before any other call on a
// file the crash left unknown; Unresolved lists them. Outside a crash, Compare
// may be called after any step, and the model-based test calls it after
// every one.
//
// An error wraps ErrDiverged when the journal did what the RFC does not allow,
// and ErrMisuse when the model was called in a way it cannot take, which is
// the caller's bug, never the journal's.
//
// decision: the model covers writes, fills, offers and their reports,
// releases, truncates, deallocates, deletes, settles, Since, crashes and
// reopens. Repack changes no content, so it has no operation here. What it
// may drop shows only after a crash, where the model allows it: a settled
// removal's marker, and the hold a record backing nothing has on the change
// version. The model leaves out snapshot holds and stamps, clone
// targets, Forget and Strand, capacity and its refusals, failed sync windows
// and loss records, and Stats. Each is checked by the conformance tests that
// own it. Add one here when the model-based test needs to drive it.
package model
