package netlogon

import (
	"context"
	"testing"
)

func TestNetworkLogonRequiresCredential(t *testing.T) {
	a := NewAuthenticator(NewOfflineProvider(MachineCredential{})) // incomplete -> Credential() errors
	_, err := a.NetworkLogon(context.Background(), NetworkLogonRequest{
		Username: "alice", Domain: "DITTOFS",
	})
	if err == nil {
		t.Fatal("expected error when machine credential is incomplete")
	}
}
