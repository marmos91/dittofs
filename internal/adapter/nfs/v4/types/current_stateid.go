package types

import "io"

// Current stateid tracking (RFC 8881 Section 16.2.3.1.2).
//
// NFSv4.1 lets one operation in a COMPOUND hand its stateid to a later one
// without the client having to name it: a stateid of seqid 1 with an all-zeros
// "other" is a placeholder meaning "the current stateid". The COMPOUND keeps
// that stateid alongside the current filehandle, under three rules:
//
//   - an operation that returns a stateid sets the current stateid to it;
//   - an operation that sets the current filehandle without returning a
//     stateid drops the current stateid;
//   - an operation that merely uses a stateid leaves it alone.
//
// SAVEFH and RESTOREFH carry the stateid along with the filehandle they save
// and restore.
//
// A COMPOUND that has no current stateid — nothing has set one, or a
// filehandle operation dropped it — answers the placeholder with
// NFS4ERR_BAD_STATEID rather than silently substituting the anonymous stateid,
// which is what RFC 8881 Section 16.2.3.1.2's last example requires and what
// the Linux server does.

// isCurrentStateidPlaceholder reports whether this stateid is the NFSv4.1
// "use the current stateid" value: seqid 1 with an all-zeros "other".
func (s *Stateid4) isCurrentStateidPlaceholder() bool {
	return s.Seqid == 1 && s.Other == [NFS4_OTHER_SIZE]byte{}
}

// tracksCurrentStateid reports whether the placeholder is meaningful in this
// COMPOUND. It is an NFSv4.1 addition; in a v4.0 COMPOUND (1, 0) is just an
// ordinary stateid the server never issued.
func (c *CompoundContext) tracksCurrentStateid() bool {
	return c.MinorVersionAccepted && c.MinorVersion >= 1
}

// SetCurrentStateid records the stateid an operation returned as the COMPOUND's
// current stateid.
func (c *CompoundContext) SetCurrentStateid(sid *Stateid4) {
	c.currentStateid = *sid
	c.hasCurrentStateid = true
}

// ClearCurrentStateid drops the current stateid, as an operation that sets the
// current filehandle without returning a stateid must.
func (c *CompoundContext) ClearCurrentStateid() {
	c.currentStateid = Stateid4{}
	c.hasCurrentStateid = false
}

// SaveCurrentStateid is SAVEFH's half of the pair: the saved filehandle takes
// the current stateid with it.
func (c *CompoundContext) SaveCurrentStateid() {
	c.savedStateid = c.currentStateid
	c.hasSavedStateid = c.hasCurrentStateid
}

// RestoreCurrentStateid is RESTOREFH's half: the restored filehandle brings
// back the stateid saved with it.
func (c *CompoundContext) RestoreCurrentStateid() {
	c.currentStateid = c.savedStateid
	c.hasCurrentStateid = c.hasSavedStateid
}

// resolveStateid substitutes the COMPOUND's current stateid for the placeholder
// value, returning the stateid the operation should act on and an NFSv4 status.
// Any other stateid — and any stateid at all in a v4.0 COMPOUND — is returned
// unchanged.
func (c *CompoundContext) resolveStateid(sid *Stateid4) (*Stateid4, uint32) {
	if !c.tracksCurrentStateid() || !sid.isCurrentStateidPlaceholder() {
		return sid, NFS4_OK
	}
	if !c.hasCurrentStateid {
		return nil, NFS4ERR_BAD_STATEID
	}
	current := c.currentStateid
	return &current, NFS4_OK
}

// DecodeStateidArg decodes a stateid4 operation argument and resolves the
// current-stateid placeholder in one step, so every operation that takes a
// stateid gets the same treatment. It returns NFS4ERR_BADXDR if the argument
// does not decode and NFS4ERR_BAD_STATEID if the placeholder names a current
// stateid the COMPOUND does not have.
func DecodeStateidArg(ctx *CompoundContext, reader io.Reader) (*Stateid4, uint32) {
	sid, err := DecodeStateid4(reader)
	if err != nil {
		return nil, NFS4ERR_BADXDR
	}
	return ctx.resolveStateid(sid)
}

// ClearsCurrentStateid reports whether an operation sets or voids the current
// filehandle without returning a stateid, which per RFC 8881 Section
// 16.2.3.1.2 drops the current stateid along with it. RESTOREFH is absent on
// purpose: it restores the saved stateid instead of dropping one.
func ClearsCurrentStateid(opCode uint32) bool {
	switch opCode {
	case OP_PUTFH, OP_PUTROOTFH, OP_PUTPUBFH,
		OP_LOOKUP, OP_LOOKUPP, OP_CREATE, OP_OPENATTR,
		OP_SECINFO, OP_SECINFO_NO_NAME:
		return true
	default:
		return false
	}
}
