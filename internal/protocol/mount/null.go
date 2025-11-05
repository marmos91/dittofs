package mount

import "github.com/marmos91/dittofs/internal/metadata"

// MountNull does nothing. This is used to test connectivity.
// RFC 1813 Appendix I
func (h *DefaultMountHandler) MountNull(repository metadata.Repository) ([]byte, error) {
	return []byte{}, nil
}
