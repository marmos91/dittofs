package acl

// ProbeBitsAll is the canonical set of MS-DTYP access-right bits probed
// against a file's DACL when computing effective access — for the SMB CREATE
// MxAc create-context response and for MAXIMUM_ALLOWED enforcement.
//
// Per MS-SMB2 §2.2.13.2, MaximalAccess must reflect actual security descriptor
// evaluation — each bit is OR'd into the result iff the ACL explicitly grants
// it to the requester. The set covers every ACE4_* file/dir right that has a
// Windows access-mask analog per MS-DTYP §2.4.3; NFSv4 mask bits share their
// bit positions with the equivalent Windows rights, so the resulting mask is
// directly usable on the SMB2 wire. The NFSv4-only retention bits
// (ACE4_WRITE_RETENTION = 0x200, ACE4_WRITE_RETENTION_HOLD = 0x400) are
// intentionally excluded because they have no representation in SMB access
// masks.
//
// Kept here (rather than duplicated in pkg/metadata and internal/adapter/smb)
// so the two consumers — metadata.CheckFileAccess (enforcement gate) and the
// SMB handler computeMaximalAccess (MxAc reply) — cannot drift.
var ProbeBitsAll = [...]uint32{
	ACE4_READ_DATA, // == ACE4_LIST_DIRECTORY
	ACE4_WRITE_DATA,
	ACE4_APPEND_DATA,
	ACE4_READ_NAMED_ATTRS,
	ACE4_WRITE_NAMED_ATTRS,
	ACE4_EXECUTE,
	ACE4_DELETE_CHILD,
	ACE4_READ_ATTRIBUTES,
	ACE4_WRITE_ATTRIBUTES,
	ACE4_DELETE,
	ACE4_READ_ACL,
	ACE4_WRITE_ACL,
	ACE4_WRITE_OWNER,
	ACE4_SYNCHRONIZE,
}

// ProbeMaskAll is the OR of every bit in ProbeBitsAll — the single-call
// probeMask for EvaluateGranted when computing the full effective-rights set
// (MAXIMUM_ALLOWED / MxAc). Kept in sync with ProbeBitsAll via package init.
var ProbeMaskAll = func() uint32 {
	var m uint32
	for _, b := range ProbeBitsAll {
		m |= b
	}
	return m
}()
