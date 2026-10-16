package kvstore

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// OpError is a namespace call's error with what the call named: the
// operation, the directory and the name. It unwraps to the error the call
// met, so errors.Is and the store's error checks see through it.
type OpError struct {
	Op   string
	Dir  entity.Handle
	Name []byte
	Err  error
}

func (e *OpError) Error() string {
	return fmt.Sprintf("%s %s/%q: %v", e.Op, e.Dir, e.Name, e.Err)
}

func (e *OpError) Unwrap() error { return e.Err }

// opError wraps *err, when set, with the operation and what it named.
func opError(err *error, op string, dir entity.Handle, name []byte) {
	if *err != nil {
		*err = &OpError{Op: op, Dir: dir, Name: name, Err: *err}
	}
}
