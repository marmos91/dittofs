package handlers

import "github.com/marmos91/dittofs/pkg/content"

type DefaultNFSHandler struct {
	ContentStore content.WritableContentStore
}
