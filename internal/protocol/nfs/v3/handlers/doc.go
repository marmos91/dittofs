package handlers

import (
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/content/cache"
)

type DefaultNFSHandler struct {
	ContentStore content.WritableContentStore
	WriteCache   cache.WriteCache
}
