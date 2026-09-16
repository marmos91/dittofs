package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// multiRoundTranslator is a compile-time guard on the interface's shape.
//
// The challenge-token return and ErrMoreProcessingRequired exist so a stateful
// mechanism (NTLM, RPCSEC_GSS, SPNEGO) can be expressed later without a
// breaking change to Translator. Narrowing Translate to two values would make
// this type stop compiling — which is the point of the guard, since the
// original design shipped a two-value signature that no multi-round mechanism
// could implement.
type multiRoundTranslator struct{ rounds int }

func (m *multiRoundTranslator) Method() string { return "stub" }

func (m *multiRoundTranslator) Translate(_ context.Context, token []byte) (*Result, []byte, error) {
	if len(token) == 0 {
		return nil, []byte("challenge"), ErrMoreProcessingRequired
	}
	return &Result{
		Identity: &metadata.Identity{Username: "stub"},
		Method:   m.Method(),
	}, nil, nil
}

func TestTranslator_MultiRoundShape(t *testing.T) {
	var tr Translator = &multiRoundTranslator{}

	_, challenge, err := tr.Translate(context.Background(), nil)
	if !errors.Is(err, ErrMoreProcessingRequired) {
		t.Fatalf("round 1 err = %v, want ErrMoreProcessingRequired", err)
	}
	if string(challenge) != "challenge" {
		t.Fatalf("round 1 challenge = %q, want challenge", challenge)
	}

	res, challenge, err := tr.Translate(context.Background(), []byte("response"))
	if err != nil {
		t.Fatalf("round 2 err = %v, want nil", err)
	}
	if challenge != nil {
		t.Fatalf("round 2 challenge = %v, want nil", challenge)
	}
	if res == nil || res.Method != "stub" {
		t.Fatalf("round 2 result = %+v, want method stub", res)
	}
}
