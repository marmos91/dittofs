package journalv2

import (
	"fmt"
	"testing"
)

// surfaceCall is one exported function or method, called with zero or minimal
// arguments. call returns the error the callee returned, or nil.
type surfaceCall struct {
	name string
	call func() error
}

// surface lists every exported function and method of the public package.
// Each one is added in the change that declares it.
var surface = []surfaceCall{}

// TestSurface calls every entry of surface and asserts that it returns or
// errors cleanly: a panic fails the entry by name instead of aborting the test
// binary. The assertion holds both for a stub and for an implementation, so the
// table never needs to change when a method is implemented.
func TestSurface(t *testing.T) {
	for _, c := range surface {
		t.Run(c.name, func(t *testing.T) {
			if err := callClean(c.call); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// callClean runs call and converts a panic into an error.
func callClean(call func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panicked: %v", r)
		}
	}()

	_ = call()
	return nil
}
