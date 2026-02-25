package callback

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// SendNotify sends an SM_NOTIFY callback to a registered client.
//
// This function builds the SM_NOTIFY status message and sends it to the client
// at the address specified in their registration. It uses the callback information
// (program, version, procedure) from the client's original SM_MON request.
//
// Per CONTEXT.md locked decisions:
//   - Fresh TCP connection for each callback
//   - 5 second TOTAL timeout for dial + I/O
//   - Caller should treat failed notification as client crash
//
// Parameters:
//   - ctx: Context for cancellation
//   - client: The callback client to use for sending
//   - reg: Client registration containing callback info
//   - serverName: This server's hostname (sent as mon_name in Status)
//   - serverState: Current server state counter
//
// Returns:
//   - nil on success
//   - error if callback fails (client is presumed crashed)
func SendNotify(
	ctx context.Context,
	client *Client,
	reg *lock.ClientRegistration,
	serverName string,
	serverState int32,
) error {
	if reg.CallbackInfo == nil {
		return fmt.Errorf("no callback info for client %s", reg.ClientID)
	}

	// Build callback address from registration
	addr := reg.CallbackInfo.Hostname

	// Build status message
	// - MonName: This server's hostname (the host that changed state)
	// - State: New server state counter
	// - Priv: Client's private data from SM_MON (returned unchanged)
	status := &types.Status{
		MonName: serverName,
		State:   serverState,
		Priv:    reg.Priv,
	}

	// Send callback using client's specified program/version/procedure
	return client.Send(
		ctx,
		addr,
		status,
		reg.CallbackInfo.Proc,
		reg.CallbackInfo.Program,
		reg.CallbackInfo.Version,
	)
}
