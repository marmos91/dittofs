package suite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

func neutralErrors(t *testing.T, newService func(t *testing.T) vfs.Service) {
	// Every call answers nil or one of the service's own errors, so an
	// adapter mapping each of vfs.Errors maps everything it is answered;
	// zero arguments, as a client can send, included.
	t.Run("EveryCallAnswersANeutralError", func(t *testing.T) {
		for _, e := range nonNeutral(t.Context(), newService(t)) {
			t.Error(e)
		}
	})
}

// nonNeutral calls every method of s with zero arguments and names each error
// answered that is none of vfs.Errors.
func nonNeutral(ctx context.Context, s vfs.Service) []string {
	var found []string
	v := reflect.ValueOf(s)
	it := reflect.TypeFor[vfs.Service]()
	for i := range it.NumMethod() {
		name := it.Method(i).Name
		m := v.MethodByName(name)
		args := []reflect.Value{reflect.ValueOf(ctx)}
		for j := 1; j < m.Type().NumIn(); j++ {
			args = append(args, reflect.Zero(m.Type().In(j)))
		}
		for _, r := range m.Call(args) {
			var errs []error
			switch e := r.Interface().(type) {
			case error:
				errs = []error{e}
			case []error:
				errs = e
			}
			for _, err := range errs {
				if err != nil && !neutral(err) {
					found = append(found, fmt.Sprintf("%s answered %v, which is none of vfs.Errors", name, err))
				}
			}
		}
	}
	return found
}

// neutral reports whether err is, or wraps, one of the service's errors.
func neutral(err error) bool {
	for _, e := range vfs.Errors() {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
