// Package authz is the filesystem's one caller of the metadata store's
// authorisation chokepoint: every Authorize and Granted the filesystem makes
// is made here, through Decide and Rights, and here the filesystem builds the
// access event the share's audit policy selects, since the chokepoint emits
// nothing.
package authz

import (
	"context"
	"errors"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// Caller is one call's caller as admission resolved it on one share, and
// what an access event names of it.
type Caller struct {
	entity.Identity               // as the chokepoint takes it: mapped, squashed, its groups and flags set
	Mapped          vfs.Principal // who the caller proved or claimed to be, before any squash; zero where it maps to none
	Share           vfs.ShareID   // the share it was admitted to
	Proved          vfs.Identity  // what the adapter proved
	Client          vfs.ClientID  // zero where the call names no client
	Protocol        vfs.Protocol  // zero where the call names no client
}

// Policy is what a share's audit policy selects.
type Policy uint8

const (
	AuditOff     Policy = iota // nothing
	AuditDenials               // every refusal
	AuditChanges               // every refusal, and every decision on a call that may change state
	AuditAll                   // every decision
)

// Authz decides through the store and emits what the audit policy selects.
type Authz struct {
	Files  kvstore.Files
	Policy func(vfs.ShareID) Policy               // nil: every share's is AuditOff
	Emit   func(context.Context, vfs.AccessEvent) // never waits
}

// Decide is the chokepoint's decision on c doing want to the file h names for
// op; via is the directory h was resolved through, or nil, which can grant
// delete; name is the entry acted on, for a namespace operation. It answers
// the store's error, which the filesystem maps. A refusal or a grant is
// audited as the share's policy selects; an error that decides nothing, such
// as a stale handle, is not.
//
// ponytail: an access event is selected by the share's policy only. The
// store's Authorize does not report an audit or alarm entry of the file's
// ACL selecting the decision, so SelectedByEntry is never emitted; emit it
// once the chokepoint reports one.
func (a Authz) Decide(ctx context.Context, c Caller, op vfs.Op, h, via vfs.Handle, name []byte, want vfs.Access) error {
	err := a.Files.Authorize(ctx, c.Identity, h, via, want)
	allowed := err == nil
	if !allowed && !Refused(err) {
		return err
	}
	if a.selects(c.Share, op, allowed) {
		_, file, _ := h.Parse()
		a.Emit(ctx, vfs.AccessEvent{
			Time: time.Now(), Share: c.Share, Client: c.Client, Addr: c.Proved.Client, Identity: c.Proved,
			Protocol: c.Protocol, Op: op, File: file, Name: name, Want: want, Allowed: allowed,
			SelectedBy: vfs.SelectedByPolicy,
		})
	}
	return err
}

// Rights is the part of want the chokepoint would grant c on the file h
// names, delete through via included.
//
// decision: Rights emits no access event. It answers a question, such as a
// tree connect's maximal access, and refuses nothing; the decision an open
// or a call makes on what it answers goes through Decide, which is
// audited. Audit it too if an audit policy is asked to record what a caller
// was told it may do.
func (a Authz) Rights(ctx context.Context, c Caller, h, via vfs.Handle, want vfs.Access) (vfs.Access, error) {
	return a.Files.Granted(ctx, c.Identity, h, via, want)
}

func (a Authz) selects(share vfs.ShareID, op vfs.Op, allowed bool) bool {
	if a.Policy == nil {
		return false
	}
	switch a.Policy(share) {
	case AuditAll:
		return true
	case AuditChanges:
		return !allowed || vfs.Mutates(op)
	case AuditDenials:
		return !allowed
	}
	return false
}

// Refused reports whether err is the chokepoint refusing the access asked:
// the grant, a read-only narrowing, the mode or the ACL, or the immutable
// and append-only flags.
func Refused(err error) bool {
	var e *merrs.StoreError
	return errors.As(err, &e) &&
		(e.Code == merrs.ErrAccessDenied || e.Code == merrs.ErrPermissionDenied || e.Code == merrs.ErrReadOnly)
}
