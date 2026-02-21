package client

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var sessionsListCmd = &cobra.Command{
	Use:   "list <client-id>",
	Short: "List sessions for a client",
	Long: `List all NFSv4.1 sessions for a given client by its hex-encoded client ID.

Examples:
  # List sessions as table
  dfsctl client sessions list 0000000100000001

  # List sessions as JSON
  dfsctl client sessions list 0000000100000001 -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionsList,
}

// SessionList is a list of sessions for table rendering.
type SessionList []apiclient.SessionInfo

// Headers implements TableRenderer.
func (sl SessionList) Headers() []string {
	return []string{"SESSION_ID", "FORE_SLOTS", "BACK_SLOTS", "BACK_CHANNEL", "CREATED_AT"}
}

// Rows implements TableRenderer.
func (sl SessionList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		// Truncate session ID for readability
		shortSID := s.SessionID
		if len(shortSID) > 16 {
			shortSID = shortSID[:16] + "..."
		}
		rows = append(rows, []string{
			shortSID,
			fmt.Sprintf("%d", s.ForeSlots),
			fmt.Sprintf("%d", s.BackSlots),
			cmdutil.BoolToYesNo(s.BackChannel),
			s.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return rows
}

func runSessionsList(cmd *cobra.Command, args []string) error {
	clientID := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	sessions, err := client.ListSessions(clientID)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, sessions, len(sessions) == 0,
		"No sessions for this client.", SessionList(sessions))
}
