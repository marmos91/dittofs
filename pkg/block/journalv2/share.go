package journalv2

// Share attaches the share whose tag is tag and returns a handle through which
// its engine works. Every record the handle appends carries tag, and a file
// belongs, for as long as any segment holds one of its records, to the tag of
// its records.
//
// floor is the larger of the share's version floor, the highest content
// version recorded for any file of the share, and the share's durable version
// limit. Before it returns, Share raises the journal's version counter above
// floor, so no write through the handle is given a version below one the
// share's files already hold, nor one issued before a crash whose record was
// lost.
//
// limit bounds the record bytes of the share's records on disk; zero means no
// limit below the journal's maximum, and a negative limit is refused with
// ErrInvalidSetting. policy is the share's sync policy. A tag may have more
// than one handle open at once.
func (j *Journal) Share(tag ShareTag, limit int64, floor Version, policy SyncPolicy) (*Handle, error) {
	return nil, ErrNotImplemented
}

// Forget drops tag whole: every held and retained extent, removal marker and
// hold mark of the tag's files. It appends one header-only forget record,
// drawn on the reserved headroom and durable before Forget returns, so
// recovery never rebuilds any of them. Nothing is left to settle, and Since
// never reports the drop.
//
// Forget is refused with ErrHandleOpen while a handle of tag is open, and with
// a NoSpaceError naming LimitHeadroom when the headroom is exhausted. It is
// idempotent: forgetting a tag that holds nothing succeeds.
//
// The journal never decides on its own which tags to forget: a tag the
// caller still records may hold the only copy of dirty content.
func (j *Journal) Forget(tag ShareTag) error {
	return ErrNotImplemented
}

// Strand marks tag as claimed by no share. From the call on, no read through
// another tag's handle is served from the tag's extents, and none of them is
// evicted, offered or released. A stranded tag stays readable through a
// handle of its own, so its extents can be exported before it is forgotten.
//
// Strand is kept in memory only: the caller names every unclaimed tag again at
// every start, before serving. It appends nothing.
func (j *Journal) Strand(tag ShareTag) error {
	return ErrNotImplemented
}

// Draw issues the next content version and appends nothing, for a change the
// caller stores elsewhere; the caller records it, with the file's floor, in
// the change's own transaction. It takes no file append lock.
//
// Draw is refused with a NoSpaceError naming LimitVersion when the next
// version would reach the version limit, and before the first
// SetVersionLimit, since the journal issues no version without a limit.
func (j *Journal) Draw() (Version, error) {
	return Version{}, ErrNotImplemented
}

// SetVersionLimit sets the version limit: no version is issued at or above
// mark. mark is the smallest of the durable per-share limits the caller has
// recorded for every share the journal serves, raised by the caller ahead of
// use so a write is not refused for want of versions.
func (j *Journal) SetVersionLimit(mark Version) {}
