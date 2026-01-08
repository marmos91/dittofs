package config

import (
	"context"
	"fmt"
	"maps"

	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"
	"github.com/marmos91/dittofs/dittofs-operator/utils/nfs"
	"github.com/marmos91/dittofs/dittofs-operator/utils/smb"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func GenerateDittoFSConfig(ctx context.Context, client client.Client, dittoServer *dittoiov1alpha1.DittoServer) (string, error) {
	metadataStores := make(map[string]MetadataStore)
	contentStores := make(map[string]ContentStore)

	for _, backend := range dittoServer.Spec.Config.Backends {
		// Resolve config values from both direct config and secret references
		resolvedConfig, err := resolveBackendConfig(ctx, client, dittoServer.Namespace, backend)
		if err != nil {
			return "", fmt.Errorf("failed to resolve config for backend %s: %w", backend.Name, err)
		}

		switch backend.Type {
		case "badger":
			metadataStores[backend.Name] = MetadataStore{
				Type: "badger",
				Badger: &BadgerConfig{
					DBPath: getConfigValue(resolvedConfig, "path", "/data/metadata"),
				},
			}
		case "local":
			contentStores[backend.Name] = ContentStore{
				Type: "filesystem",
				Filesystem: &FilesystemConfig{
					Path: getConfigValue(resolvedConfig, "path", "/data/content"),
				},
			}
		case "s3":
			contentStores[backend.Name] = ContentStore{
				Type: "s3",
				S3: &S3Config{
					Bucket:          getConfigValue(resolvedConfig, "bucket", ""),
					Region:          getConfigValue(resolvedConfig, "region", "us-east-1"),
					Endpoint:        getConfigValue(resolvedConfig, "endpoint", ""),
					AccessKeyID:     getConfigValue(resolvedConfig, "access_key_id", ""),
					SecretAccessKey: getConfigValue(resolvedConfig, "secret_access_key", ""),
					ForcePathStyle:  getConfigValue(resolvedConfig, "force_path_style", "") == "true",
				},
			}
		default:
			return "", fmt.Errorf("unsupported backend type %q for backend %q", backend.Type, backend.Name)
		}
	}

	cacheStores := make(map[string]CacheStore)
	for _, cache := range dittoServer.Spec.Config.Caches {
		cacheStore := CacheStore{
			Type: cache.Type,
		}

		if cache.Memory != nil {
			memoryConfig := make(map[string]any)
			if cache.Memory.MaxSize != "" {
				memoryConfig["max_size"] = parseSizeString(cache.Memory.MaxSize)
			}
			if len(memoryConfig) > 0 {
				cacheStore.Memory = memoryConfig
			}
		}

		if cache.Prefetch != nil {
			prefetch := &Prefetch{}
			if cache.Prefetch.Enabled != nil {
				prefetch.Enabled = cache.Prefetch.Enabled
			}
			if cache.Prefetch.MaxFileSize != "" {
				prefetch.MaxFileSize = parseSizeString(cache.Prefetch.MaxFileSize)
			}
			if cache.Prefetch.ChunkSize != "" {
				prefetch.ChunkSize = parseSizeString(cache.Prefetch.ChunkSize)
			}
			cacheStore.Prefetch = prefetch
		}

		if cache.Flusher != nil {
			flusher := &Flusher{}
			if cache.Flusher.Interval != "" {
				flusher.SweepInterval = cache.Flusher.Interval
			}
			if cache.Flusher.FlushTimeout != "" {
				flusher.FlushTimeout = cache.Flusher.FlushTimeout
			}
			if cache.Flusher.Workers != nil {
				flusher.FlushPoolSize = cache.Flusher.Workers
			}
			cacheStore.Flusher = flusher
		}

		cacheStores[cache.Name] = cacheStore
	}

	shares := make([]Share, 0, len(dittoServer.Spec.Config.Shares))
	for _, share := range dittoServer.Spec.Config.Shares {
		allowedAuthMethods := share.AllowedAuthMethods
		if len(allowedAuthMethods) == 0 {
			allowedAuthMethods = []string{"anonymous", "unix"}
		}

		identityMapping := IdentityMapping{
			MapAllToAnonymous:        false,
			MapPrivilegedToAnonymous: false,
			AnonymousUID:             65534,
			AnonymousGID:             65534,
		}
		if share.IdentityMapping != nil {
			identityMapping.MapAllToAnonymous = share.IdentityMapping.MapAllToAnonymous
			identityMapping.MapPrivilegedToAnonymous = share.IdentityMapping.MapPrivilegedToAnonymous
			if share.IdentityMapping.AnonymousUID != 0 {
				identityMapping.AnonymousUID = share.IdentityMapping.AnonymousUID
			}
			if share.IdentityMapping.AnonymousGID != 0 {
				identityMapping.AnonymousGID = share.IdentityMapping.AnonymousGID
			}
		}

		rootDirAttrs := DirectoryAttributes{
			Mode: 0755,
			UID:  0,
			GID:  0,
		}
		if share.RootDirectoryAttributes != nil {
			if share.RootDirectoryAttributes.Mode != 0 {
				rootDirAttrs.Mode = share.RootDirectoryAttributes.Mode
			}
			rootDirAttrs.UID = share.RootDirectoryAttributes.UID
			rootDirAttrs.GID = share.RootDirectoryAttributes.GID
		}

		// Set AllowGuest based on configuration, defaulting to false
		allowGuest := false
		if share.AllowGuest != nil {
			allowGuest = *share.AllowGuest
		}

		if share.RequireAuth && allowGuest {
			fmt.Printf("warning: share %q has RequireAuth enabled but AllowGuest is true; guest access will still be permitted\n", share.ExportPath)
		}

		// Set DefaultPermission based on configuration, defaulting to "read"
		defaultPermission := "read"
		if share.DefaultPermission != "" {
			defaultPermission = share.DefaultPermission
		}

		shareYAML := Share{
			Name:                    share.ExportPath,
			MetadataStore:           share.MetadataStore,
			ContentStore:            share.ContentStore,
			Cache:                   share.Cache,
			ReadOnly:                share.ReadOnly,
			AllowGuest:              allowGuest,
			DefaultPermission:       defaultPermission,
			AllowedClients:          share.AllowedClients,
			DeniedClients:           share.DeniedClients,
			RequireAuth:             share.RequireAuth,
			AllowedAuthMethods:      allowedAuthMethods,
			IdentityMapping:         identityMapping,
			RootDirectoryAttributes: rootDirAttrs,
			DumpRestricted:          share.DumpRestricted,
			DumpAllowedClients:      share.DumpAllowedClients,
		}

		shares = append(shares, shareYAML)
	}

	// Build user management configuration
	var users []User
	var groups []Group
	var guest *Guest

	if dittoServer.Spec.Users != nil {
		// Resolve users with secret support
		for _, userSpec := range dittoServer.Spec.Users.Users {
			user, err := resolveUserConfig(ctx, client, dittoServer.Namespace, userSpec)
			if err != nil {
				return "", fmt.Errorf("failed to resolve config for user %s: %w", userSpec.Username, err)
			}
			users = append(users, user)
		}

		for _, g := range dittoServer.Spec.Users.Groups {
			groups = append(groups, Group{
				Name:             g.Name,
				GID:              g.GID,
				SharePermissions: g.SharePermissions,
			})
		}

		if dittoServer.Spec.Users.Guest != nil {
			guest = &Guest{
				Enabled:          dittoServer.Spec.Users.Guest.Enabled,
				UID:              dittoServer.Spec.Users.Guest.UID,
				GID:              dittoServer.Spec.Users.Guest.GID,
				SharePermissions: dittoServer.Spec.Users.Guest.SharePermissions,
			}
		}
	}

	adapters := AdaptersConfig{
		NFS: NFSAdapter{
			Enabled:        true,
			Port:           nfs.GetNFSPort(dittoServer),
			MaxConnections: 0,
			Timeouts: TimeoutsConfig{
				Read:     "5m0s",
				Write:    "30s",
				Idle:     "5m0s",
				Shutdown: "30s",
			},
			MetricsLogInterval: "5m0s",
		},
	}

	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Enabled {
		smbAdapter := SMBAdapter{
			Enabled:                  true,
			Port:                     smb.GetSMBPort(dittoServer),
			MaxConnections:           smb.GetMaxConnections(dittoServer),
			MaxRequestsPerConnection: smb.GetMaxRequestsPerConnection(dittoServer),
			Timeouts: TimeoutsConfig{
				Read:     smb.GetTimeout(dittoServer.Spec.SMB.Timeouts, "read", "5m0s"),
				Write:    smb.GetTimeout(dittoServer.Spec.SMB.Timeouts, "write", "30s"),
				Idle:     smb.GetTimeout(dittoServer.Spec.SMB.Timeouts, "idle", "5m0s"),
				Shutdown: smb.GetTimeout(dittoServer.Spec.SMB.Timeouts, "shutdown", "30s"),
			},
			MetricsLogInterval: smb.GetMetricsLogInterval(dittoServer),
		}

		if dittoServer.Spec.SMB.Credits != nil {
			smbAdapter.Credits = &SMBCredits{
				Strategy:                  smb.GetCreditsStrategy(dittoServer),
				MinGrant:                  smb.GetCreditsMinGrant(dittoServer),
				MaxGrant:                  smb.GetCreditsMaxGrant(dittoServer),
				InitialGrant:              smb.GetCreditsInitialGrant(dittoServer),
				MaxSessionCredits:         smb.GetCreditsMaxSessionCredits(dittoServer),
				LoadThresholdHigh:         smb.GetCreditsLoadThresholdHigh(dittoServer),
				LoadThresholdLow:          smb.GetCreditsLoadThresholdLow(dittoServer),
				AggressiveClientThreshold: smb.GetCreditsAggressiveClientThreshold(dittoServer),
			}
		}

		adapters.SMB = smbAdapter
	}

	config := DittoFSConfig{
		Logging: LoggingConfig{
			Level:  "INFO",
			Format: "text",
			Output: "stdout",
		},
		Server: ServerConfig{
			ShutdownTimeout: "30s",
		},
		Metadata: MetadataConfig{
			Global: MetadataGlobal{
				FilesystemCapabilities: FilesystemCapabilities{
					MaxReadSize:        1048576,
					PreferredReadSize:  65536,
					MaxWriteSize:       1048576,
					PreferredWriteSize: 65536,
					MaxFileSize:        9223372036854775807,
					MaxFilenameLen:     255,
					MaxPathLen:         4096,
					MaxHardLinkCount:   32767,
					SupportsHardLinks:  true,
					SupportsSymlinks:   true,
					CaseSensitive:      true,
					CasePreserving:     true,
				},
			},
			Stores: metadataStores,
		},
		Content: ContentConfig{
			Global: map[string]any{},
			Stores: contentStores,
		},
		Cache: CacheConfig{
			Stores: cacheStores,
		},
		Shares:   shares,
		Users:    users,
		Groups:   groups,
		Guest:    guest,
		Adapters: adapters,
	}

	yamlBytes, err := yaml.Marshal(&config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	return string(yamlBytes), nil
}

func getConfigValue(config map[string]string, key, defaultValue string) string {
	if val, ok := config[key]; ok {
		return val
	}
	return defaultValue
}

// resolveBackendConfig resolves configuration values from both direct config and secret references
func resolveBackendConfig(ctx context.Context, client client.Client, namespace string, backend dittoiov1alpha1.BackendConfig) (map[string]string, error) {
	resolved := make(map[string]string)

	// Copy direct config values
	maps.Copy(resolved, backend.Config)

	// Resolve secret references (they override direct config values)
	for key, secretRef := range backend.SecretRefs {
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      secretRef.Name,
			Namespace: namespace,
		}

		if err := client.Get(ctx, secretKey, secret); err != nil {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretRef.Name, err)
		}

		if secretValue, ok := secret.Data[secretRef.Key]; ok {
			resolved[key] = string(secretValue)
		} else {
			return nil, fmt.Errorf("key %s not found in secret %s", secretRef.Key, secretRef.Name)
		}
	}

	return resolved, nil
}

// resolveUserConfig resolves user configuration including password from secrets
func resolveUserConfig(ctx context.Context, client client.Client, namespace string, userSpec dittoiov1alpha1.UserSpec) (User, error) {
	user := User{
		Username:         userSpec.Username,
		Enabled:          userSpec.Enabled,
		UID:              userSpec.UID,
		GID:              userSpec.GID,
		Groups:           userSpec.Groups,
		SharePermissions: make(map[string]string),
	}

	// Copy share permissions
	maps.Copy(user.SharePermissions, userSpec.SharePermissions)

	// Resolve password hash
	if userSpec.PasswordSecretRef != nil {
		// Use secret reference
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      userSpec.PasswordSecretRef.Name,
			Namespace: namespace,
		}

		if err := client.Get(ctx, secretKey, secret); err != nil {
			return user, fmt.Errorf("failed to get secret %s: %w", userSpec.PasswordSecretRef.Name, err)
		}

		if passwordHash, ok := secret.Data[userSpec.PasswordSecretRef.Key]; ok {
			user.PasswordHash = string(passwordHash)
		} else {
			return user, fmt.Errorf("key %s not found in secret %s", userSpec.PasswordSecretRef.Key, userSpec.PasswordSecretRef.Name)
		}
	} else if userSpec.PasswordHash != "" {
		// Use direct password hash (deprecated but still supported)
		user.PasswordHash = userSpec.PasswordHash
	} else {
		return user, fmt.Errorf("no password hash provided for user %s (use either passwordHash or passwordSecretRef)", userSpec.Username)
	}

	return user, nil
}

// parseSizeString converts Kubernetes-style size strings (e.g., "1Gi", "512Mi", "100Ki")
// to bytes as uint64 for DittoFS configuration.
// Returns 0 for empty or "0" strings.
// Returns the string as-is if parsing fails.
//
// TODO: make ditto compliant with kubernetes
// resources to delete this function
func parseSizeString(size string) any {
	if size == "" || size == "0" {
		return uint64(0)
	}

	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		// If parsing fails, return the string as-is
		// This allows DittoFS to handle it or fail gracefully
		return size
	}

	bytes := quantity.Value()
	if bytes < 0 {
		return size // Invalid negative size, return string
	}

	return uint64(bytes)
}
