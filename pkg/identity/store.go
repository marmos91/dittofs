package identity

import "context"

// IdentityLink represents a link between an external identity and a DittoFS user.
// The composite key is (ProviderName, ExternalID).
type IdentityLink struct {
	ProviderName string
	ExternalID   string
	Username     string
}

// LinkStore provides CRUD for external identity → DittoFS user links.
//
// The composite key (ProviderName, ExternalID) supports linked identities:
// one DittoFS user can have multiple external identities across providers.
type LinkStore interface {
	GetLink(ctx context.Context, provider, externalID string) (username string, found bool, err error)
	ListLinks(ctx context.Context, provider string) ([]IdentityLink, error)
	CreateLink(ctx context.Context, link IdentityLink) error
	DeleteLink(ctx context.Context, provider, externalID string) error
	ListLinksForUser(ctx context.Context, username string) ([]IdentityLink, error)
}

// FuncLinkStore adapts function callbacks to the LinkStore interface.
// Constructed at wiring time to bridge the controlplane store without circular imports.
// Methods with nil function fields return ErrNotConfigured instead of panicking.
type FuncLinkStore struct {
	GetLinkFn          func(ctx context.Context, provider, externalID string) (string, bool, error)
	ListLinksFn        func(ctx context.Context, provider string) ([]IdentityLink, error)
	CreateLinkFn       func(ctx context.Context, link IdentityLink) error
	DeleteLinkFn       func(ctx context.Context, provider, externalID string) error
	ListLinksForUserFn func(ctx context.Context, username string) ([]IdentityLink, error)
}

func (f *FuncLinkStore) GetLink(ctx context.Context, provider, externalID string) (string, bool, error) {
	if f.GetLinkFn == nil {
		return "", false, ErrNotConfigured
	}
	return f.GetLinkFn(ctx, provider, externalID)
}

func (f *FuncLinkStore) ListLinks(ctx context.Context, provider string) ([]IdentityLink, error) {
	if f.ListLinksFn == nil {
		return nil, ErrNotConfigured
	}
	return f.ListLinksFn(ctx, provider)
}

func (f *FuncLinkStore) CreateLink(ctx context.Context, link IdentityLink) error {
	if f.CreateLinkFn == nil {
		return ErrNotConfigured
	}
	return f.CreateLinkFn(ctx, link)
}

func (f *FuncLinkStore) DeleteLink(ctx context.Context, provider, externalID string) error {
	if f.DeleteLinkFn == nil {
		return ErrNotConfigured
	}
	return f.DeleteLinkFn(ctx, provider, externalID)
}

func (f *FuncLinkStore) ListLinksForUser(ctx context.Context, username string) ([]IdentityLink, error) {
	if f.ListLinksForUserFn == nil {
		return nil, ErrNotConfigured
	}
	return f.ListLinksForUserFn(ctx, username)
}
