package vfs

import (
	"context"
	"net/netip"
	"time"
)

// Events is the node's event sink. The filesystem builds and emits every
// event and never waits on the sink: an event the sink cannot queue is
// dropped and counted there, never failing the operation.
type Events interface {
	Quota(ctx context.Context, e QuotaEvent)
	Access(ctx context.Context, e AccessEvent)
}

// AccessEvent is one authorisation decision the share's audit policy, or an
// audit entry of the file's ACL, selected.
type AccessEvent struct {
	Time       time.Time
	Share      ShareID
	Client     ClientID // zero for NFSv3
	Addr       netip.Addr
	Identity   Identity
	Protocol   Protocol
	Op         Op
	File       FileID
	Name       []byte // the name acted on, for a namespace operation
	Want       Access
	Allowed    bool
	SelectedBy Selector
}

// Selector is what selected an access event.
type Selector uint8

const (
	SelectedByPolicy Selector = iota + 1 // the share's audit policy
	SelectedByEntry                      // an audit or alarm entry of the file's ACL
)

// QuotaEvent is one quota transition, emitted once, or a hard refusal, which
// the sink coalesces per share, scope, subject and resource.
type QuotaEvent struct {
	Time     time.Time
	Share    ShareID
	Scope    QuotaScope
	Subject  Principal // the user or group; zero for a project or share quota
	Project  uint64    // the tree; zero otherwise
	Resource QuotaResource
	Kind     QuotaEventKind
	Used     int64 // the usage judged, in the resource's unit
	Soft     int64
	Hard     int64
	GraceEnd time.Time // zero when no grace runs
	Writer   Identity  // the refused caller, for QuotaHardRefused only
}

// QuotaResource is what a quota counts.
type QuotaResource uint8

const (
	QuotaBytes QuotaResource = iota + 1
	QuotaFiles
)

// QuotaEventKind is what happened to a quota.
type QuotaEventKind uint8

const (
	QuotaAdvisoryCrossed QuotaEventKind = iota + 1
	QuotaSoftExceeded
	QuotaGraceExpired
	QuotaHardRefused
	QuotaBackUnder
)
