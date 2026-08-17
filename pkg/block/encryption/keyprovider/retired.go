package keyprovider

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
)

// retiredKeyLoader resolves one configured retired-key reference (a file
// path for the local provider, a uid for KMIP) into the master key id it
// records and its 32-byte material.
type retiredKeyLoader func(ref string) (masterKeyID string, key []byte, err error)

// loadRetiredKeys builds the decrypt-only id → key map from the
// configured references.
//
// A reference that fails to load is logged and skipped rather than
// failing construction: the frames wrapped under it become unreadable,
// which is the state the daemon was already in before the key was
// configured, and refusing to start would take every other share's data
// offline with it. A reference that resolves to an id already in use is
// a config error and does fail construction, because two keys claiming
// one id makes which one Unwrap picks a coin toss.
func loadRetiredKeys(currentID string, refs []string, load retiredKeyLoader) (map[string][]byte, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	retired := make(map[string][]byte, len(refs))
	for _, ref := range refs {
		masterKeyID, key, err := load(ref)
		if err != nil {
			logger.Warn("retired master key unreadable, blocks wrapped under it cannot be decrypted",
				"ref", ref, "error", err)
			continue
		}
		if masterKeyID == currentID {
			return nil, fmt.Errorf("%w: retired key %q has the same id %q as the current key",
				ErrInvalidConfig, ref, masterKeyID)
		}
		if _, dup := retired[masterKeyID]; dup {
			return nil, fmt.Errorf("%w: two retired keys share the id %q (second was %q)",
				ErrInvalidConfig, masterKeyID, ref)
		}
		retired[masterKeyID] = key
	}
	if len(retired) == 0 {
		return nil, nil
	}
	return retired, nil
}
