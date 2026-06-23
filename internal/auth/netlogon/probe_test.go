package netlogon

import (
	"testing"

	_ "github.com/oiweiwei/go-msrpc/dcerpc"
	_ "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	_ "github.com/oiweiwei/go-msrpc/ssp/credential"
	_ "github.com/oiweiwei/go-msrpc/ssp/gssapi"
	_ "github.com/oiweiwei/go-msrpc/ssp/ntlm"
)

func TestGoMsRpcImport(t *testing.T) {}
