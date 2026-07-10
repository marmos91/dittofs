package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/spf13/cobra"
)

var (
	createName              string
	createMetadata          string
	createLocal             string
	createRemote            string
	createReadOnly          bool
	createEncryptData       bool
	createDefaultPermission string
	createSquash            string
	createOwner             string
	createDescription       string
	createRetention         string
	createRetentionTTL      string
	createLocalStoreSize    string
	createReadBufferSize    string
	createQuotaBytes        string
	createAclCanonicalize   bool
	createAccessBasedEnum   bool
	createChangeNotifyOff   bool
	createStreamsDisabled   bool
	createContinuousAvail   bool
	createAllowMFsymlink    bool
	createEnableTrash       bool
	createTrashRetention    int
	createTrashRestrictAdm  bool
	createTrashMaxSize      int64
	createTrashExclude      []string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new share",
	Long: `Create a new share on the DittoFS server.

A share requires a metadata store and a local block store. A remote block store
is optional and enables tiered storage (local cache + remote durable storage).

Examples:
  # Create a share with local block store only
  dfsctl share create --name /data --metadata default --local fs-cache

  # Create a share with local and remote block stores
  dfsctl share create --name /archive --metadata default --local fs-cache --remote s3-store

  # Create a read-only share
  dfsctl share create --name /readonly --metadata default --local fs-cache --read-only

  # Create with default permission allowing all users read-write access
  dfsctl share create --name /shared --metadata default --local fs-cache --remote s3-store --default-permission read-write

  # Create with description
  dfsctl share create --name /docs --metadata default --local fs-cache --description "Documentation files"

  # Create a pinned share (blocks never evicted)
  dfsctl share create --name /edge-data --metadata default --local fs-cache --retention pin

  # Create with TTL retention (evict after 72 hours of no access)
  dfsctl share create --name /logs --metadata default --local fs-cache --retention ttl --retention-ttl 72h

  # Create with per-share cache size overrides
  dfsctl share create --name /bigdata --metadata default --local fs-cache --local-store-size 10GiB --read-buffer-size 2GiB

  # Create with per-share quota
  dfsctl share create --name /limited --metadata default --local fs-cache --quota-bytes 10GiB

  # Create an export that does not squash root (e.g. for root-mounted/benchmark clients)
  dfsctl share create --name /export --metadata default --local fs-cache --squash none`,
	RunE: runCreate,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Share name/path (required)")
	createCmd.Flags().StringVar(&createMetadata, "metadata", "", "Metadata store name (required)")
	createCmd.Flags().StringVar(&createLocal, "local", "", "Local block store name (required)")
	createCmd.Flags().StringVar(&createRemote, "remote", "", "Remote block store name (optional)")
	createCmd.Flags().BoolVar(&createReadOnly, "read-only", false, "Make share read-only")
	createCmd.Flags().BoolVar(&createEncryptData, "encrypt-data", false, "Require SMB3 encryption for this share")
	createCmd.Flags().StringVar(&createDefaultPermission, "default-permission", "none", "Default permission for unmapped UIDs (none|read|read-write|admin)")
	createCmd.Flags().StringVar(&createSquash, "squash", "", "NFS export squash mode (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest). Default root_to_guest (root_squash); use none or root_to_admin so a root-mounted client is not squashed to guest.")
	createCmd.Flags().StringVar(&createOwner, "owner", "", "Username that owns the share's root directory (defaults to root). The owner can write at the share root; other principals are governed by POSIX mode plus their share permission grant.")
	createCmd.Flags().StringVar(&createDescription, "description", "", "Share description")
	createCmd.Flags().StringVar(&createRetention, "retention", "", "Retention policy (pin|ttl|lru)")
	createCmd.Flags().StringVar(&createRetentionTTL, "retention-ttl", "", "Retention TTL duration (e.g., 72h, 24h)")
	createCmd.Flags().StringVar(&createLocalStoreSize, "local-store-size", "", "Per-share disk cache size override (e.g., 10GiB, 500MiB)")
	createCmd.Flags().StringVar(&createReadBufferSize, "read-buffer-size", "", "Per-share read buffer size override (e.g., 2GiB, 256MiB)")
	createCmd.Flags().StringVar(&createQuotaBytes, "quota-bytes", "", "Per-share byte quota (e.g., '10GiB', '500MiB'). 0 = unlimited (default)")
	createCmd.Flags().BoolVar(&createAclCanonicalize, "acl-canonicalize-inherited", true, "When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba \"acl flag inherited canonicalization = no\"). Default true matches Windows.")
	createCmd.Flags().BoolVar(&createAccessBasedEnum, "access-based-enumeration", false, "Enable Windows access-based enumeration (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM). When true, SMB clients only see directory entries they can read.")
	createCmd.Flags().BoolVar(&createChangeNotifyOff, "change-notify-disabled", false, "Reject SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED on this share (mirrors Samba 'kernel change notify = no').")
	createCmd.Flags().BoolVar(&createStreamsDisabled, "streams-disabled", false, "Reject SMB2 Alternate Data Stream opens with STATUS_OBJECT_NAME_INVALID on this share (mirrors Samba 'smbd:streams = no').")
	createCmd.Flags().BoolVar(&createContinuousAvail, "continuous-availability", false, "Advertise SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and allow SMB3 persistent durable handles on this share.")
	createCmd.Flags().BoolVar(&createAllowMFsymlink, "allow-mfsymlink", false, "Convert 1067-byte XSym (Minshall+French) symlink files written by macOS/Windows SMB clients into real symlinks on CLOSE. Off by default (XSym files are stored as regular files).")
	createCmd.Flags().BoolVar(&createEnableTrash, "enable-trash", false, "Enable the per-share recycle bin so deletes move to #recycle instead of being permanent.")
	createCmd.Flags().IntVar(&createTrashRetention, "trash-retention-days", 0, "Days to retain recycled items before the reaper purges them (0 = keep forever).")
	createCmd.Flags().BoolVar(&createTrashRestrictAdm, "trash-restrict-empty-to-admin", false, "Restrict emptying the recycle bin to admins.")
	createCmd.Flags().Int64Var(&createTrashMaxSize, "trash-max-size", 0, "Max bytes the recycle bin may hold before the reaper evicts oldest items (0 = unbounded).")
	createCmd.Flags().StringSliceVar(&createTrashExclude, "trash-exclude", nil, "Glob patterns whose deletions bypass the recycle bin (repeatable).")
	_ = createCmd.MarkFlagRequired("local")
}

func runCreate(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	name := createName
	if name == "" {
		name, err = prompt.InputRequired("Share name (e.g., /export)")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	metadata := createMetadata
	if metadata == "" {
		metadata, err = prompt.InputRequired("Metadata store name")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	local := createLocal
	if local == "" {
		local, err = prompt.InputRequired("Local block store name")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	remote := createRemote
	if remote == "" && !cmd.Flags().Changed("remote") && createName == "" {
		// Interactive mode - ask for optional remote store
		remote, err = prompt.InputOptional("Remote block store name (optional, Enter to skip)")
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	defaultPerm := createDefaultPermission
	if !cmd.Flags().Changed("default-permission") && createName == "" {
		// Interactive mode - ask for default permission
		permOptions := []string{"read-write", "read", "admin", "none"}
		selectedPerm, err := prompt.SelectString("Default permission", permOptions)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		defaultPerm = selectedPerm
	}

	req := &apiclient.CreateShareRequest{
		Name:              name,
		MetadataStoreID:   metadata,
		LocalBlockStore:   local,
		ReadOnly:          createReadOnly,
		EncryptData:       createEncryptData,
		DefaultPermission: defaultPerm,
		Owner:             createOwner,
		Description:       createDescription,
	}
	if remote != "" {
		req.RemoteBlockStore = &remote
	}
	if createRetention != "" {
		req.RetentionPolicy = createRetention
	}
	if createRetentionTTL != "" {
		req.RetentionTTL = createRetentionTTL
	}
	if createLocalStoreSize != "" {
		req.LocalStoreSize = createLocalStoreSize
	}
	if createReadBufferSize != "" {
		req.ReadBufferSize = createReadBufferSize
	}
	if createQuotaBytes != "" {
		req.QuotaBytes = createQuotaBytes
	}
	// Refs #514: only send when explicitly set so the server applies its
	// own default (true) on unset.
	if cmd.Flags().Changed("acl-canonicalize-inherited") {
		v := createAclCanonicalize
		req.AclFlagInheritedCanonicalization = &v
	}
	// Refs #532: same pattern — only forward when the operator set it.
	if cmd.Flags().Changed("access-based-enumeration") {
		v := createAccessBasedEnum
		req.AccessBasedEnumeration = &v
	}
	if cmd.Flags().Changed("change-notify-disabled") {
		v := createChangeNotifyOff
		req.ChangeNotifyDisabled = &v
	}
	if cmd.Flags().Changed("streams-disabled") {
		v := createStreamsDisabled
		req.StreamsDisabled = &v
	}
	if cmd.Flags().Changed("continuous-availability") {
		v := createContinuousAvail
		req.ContinuousAvailability = &v
	}
	if cmd.Flags().Changed("allow-mfsymlink") {
		v := createAllowMFsymlink
		req.AllowMFsymlink = &v
	}
	// Per-share recycle-bin policy (#190): only forward flags the operator
	// set so the server applies its own defaults (trash disabled, zero
	// limits) on unset.
	if cmd.Flags().Changed("enable-trash") {
		v := createEnableTrash
		req.TrashEnabled = &v
	}
	if cmd.Flags().Changed("trash-retention-days") {
		v := createTrashRetention
		req.TrashRetentionDays = &v
	}
	if cmd.Flags().Changed("trash-restrict-empty-to-admin") {
		v := createTrashRestrictAdm
		req.TrashRestrictToAdmin = &v
	}
	if cmd.Flags().Changed("trash-max-size") {
		v := createTrashMaxSize
		req.TrashMaxBytes = &v
	}
	if cmd.Flags().Changed("trash-exclude") {
		req.TrashExcludePatterns = createTrashExclude
	}

	// Squash lives on the NFS adapter config endpoint, not the share record.
	// Validate up front so we don't create a share and then fail to apply a
	// bogus squash mode, leaving it half-configured.
	if cmd.Flags().Changed("squash") && !models.SquashMode(createSquash).IsValid() {
		return fmt.Errorf("--squash: invalid value %q, must be one of none|root_to_admin|root_to_guest|all_to_admin|all_to_guest", createSquash)
	}

	share, err := client.CreateShare(req)
	if err != nil {
		return fmt.Errorf("failed to create share: %w", err)
	}

	if cmd.Flags().Changed("squash") {
		if _, err := client.PatchShareNFSConfig(share.Name, &apiclient.PatchShareNFSConfigRequest{Squash: &createSquash}); err != nil {
			return fmt.Errorf("share '%s' created, but failed to set NFS squash mode: %w", share.Name, err)
		}
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share '%s' created successfully", share.Name))
}
