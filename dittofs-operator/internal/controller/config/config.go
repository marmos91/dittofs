package config

import (
	"fmt"

	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

func GenerateDittoFSConfig(dittoServer *dittoiov1alpha1.DittoServer) (string, error) {
	metadataStores := make(map[string]MetadataStore)
	contentStores := make(map[string]ContentStore)

	for _, backend := range dittoServer.Spec.Config.Backends {
		switch backend.Type {
		case "badger":
			metadataStores[backend.Name] = MetadataStore{
				Type: "badger",
				Badger: &BadgerConfig{
					DBPath: getConfigValue(backend.Config, "path", "/data/metadata"),
				},
			}
		case "local":
			contentStores[backend.Name] = ContentStore{
				Type: "filesystem",
				Filesystem: &FilesystemConfig{
					Path: getConfigValue(backend.Config, "path", "/data/content"),
				},
			}
		case "s3":
			contentStores[backend.Name] = ContentStore{
				Type: "s3",
				S3: &S3Config{
					Bucket:          getConfigValue(backend.Config, "bucket", ""),
					Region:          getConfigValue(backend.Config, "region", "us-east-1"),
					Endpoint:        getConfigValue(backend.Config, "endpoint", ""),
					AccessKeyID:     getConfigValue(backend.Config, "access_key_id", ""),
					SecretAccessKey: getConfigValue(backend.Config, "secret_access_key", ""),
					ForcePathStyle:  getConfigValue(backend.Config, "force_path_style", "") == "true",
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

		shareYAML := Share{
			Name:                    share.ExportPath,
			MetadataStore:           share.MetadataStore,
			ContentStore:            share.ContentStore,
			Cache:                   share.Cache,
			ReadOnly:                share.ReadOnly,
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
		for _, u := range dittoServer.Spec.Users.Users {
			users = append(users, User{
				Username:         u.Username,
				PasswordHash:     u.PasswordHash,
				Enabled:          u.Enabled,
				UID:              u.UID,
				GID:              u.GID,
				Groups:           u.Groups,
				SharePermissions: u.SharePermissions,
			})
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
			Port:           getNFSPort(dittoServer),
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
			Port:                     getSMBPort(dittoServer),
			MaxConnections:           getSMBMaxConnections(dittoServer),
			MaxRequestsPerConnection: getSMBMaxRequestsPerConnection(dittoServer),
			Timeouts: TimeoutsConfig{
				Read:     getSMBTimeout(dittoServer.Spec.SMB.Timeouts, "read", "5m0s"),
				Write:    getSMBTimeout(dittoServer.Spec.SMB.Timeouts, "write", "30s"),
				Idle:     getSMBTimeout(dittoServer.Spec.SMB.Timeouts, "idle", "5m0s"),
				Shutdown: getSMBTimeout(dittoServer.Spec.SMB.Timeouts, "shutdown", "30s"),
			},
			MetricsLogInterval: getSMBMetricsLogInterval(dittoServer),
		}

		if dittoServer.Spec.SMB.Credits != nil {
			smbAdapter.Credits = &SMBCredits{
				Strategy:                  getSMBCreditsStrategy(dittoServer),
				MinGrant:                  getSMBCreditsMinGrant(dittoServer),
				MaxGrant:                  getSMBCreditsMaxGrant(dittoServer),
				InitialGrant:              getSMBCreditsInitialGrant(dittoServer),
				MaxSessionCredits:         getSMBCreditsMaxSessionCredits(dittoServer),
				LoadThresholdHigh:         getSMBCreditsLoadThresholdHigh(dittoServer),
				LoadThresholdLow:          getSMBCreditsLoadThresholdLow(dittoServer),
				AggressiveClientThreshold: getSMBCreditsAggressiveClientThreshold(dittoServer),
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

func getNFSPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.NFSPort != nil {
		return *dittoServer.Spec.NFSPort
	}
	return 2049
}

func getConfigValue(config map[string]string, key, defaultValue string) string {
	if val, ok := config[key]; ok {
		return val
	}
	return defaultValue
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

// SMB helper functions
func getSMBPort(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Port != nil {
		return *dittoServer.Spec.SMB.Port
	}
	return 445
}

func getSMBMaxConnections(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxConnections != nil {
		return *dittoServer.Spec.SMB.MaxConnections
	}
	return 0
}

func getSMBMaxRequestsPerConnection(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MaxRequestsPerConnection != nil {
		return *dittoServer.Spec.SMB.MaxRequestsPerConnection
	}
	return 100
}

func getSMBTimeout(timeouts *dittoiov1alpha1.SMBTimeoutsSpec, field, defaultValue string) string {
	if timeouts == nil {
		return defaultValue
	}
	switch field {
	case "read":
		if timeouts.Read != "" {
			return timeouts.Read
		}
	case "write":
		if timeouts.Write != "" {
			return timeouts.Write
		}
	case "idle":
		if timeouts.Idle != "" {
			return timeouts.Idle
		}
	case "shutdown":
		if timeouts.Shutdown != "" {
			return timeouts.Shutdown
		}
	}
	return defaultValue
}

func getSMBMetricsLogInterval(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.MetricsLogInterval != "" {
		return dittoServer.Spec.SMB.MetricsLogInterval
	}
	return "5m0s"
}

func getSMBCreditsStrategy(dittoServer *dittoiov1alpha1.DittoServer) string {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.Strategy != "" {
		return dittoServer.Spec.SMB.Credits.Strategy
	}
	return "adaptive"
}

func getSMBCreditsMinGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.MinGrant != nil {
		return *dittoServer.Spec.SMB.Credits.MinGrant
	}
	return 16
}

func getSMBCreditsMaxGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.MaxGrant != nil {
		return *dittoServer.Spec.SMB.Credits.MaxGrant
	}
	return 8192
}

func getSMBCreditsInitialGrant(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.InitialGrant != nil {
		return *dittoServer.Spec.SMB.Credits.InitialGrant
	}
	return 256
}

func getSMBCreditsMaxSessionCredits(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.MaxSessionCredits != nil {
		return *dittoServer.Spec.SMB.Credits.MaxSessionCredits
	}
	return 65535
}

func getSMBCreditsLoadThresholdHigh(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.LoadThresholdHigh != nil {
		return *dittoServer.Spec.SMB.Credits.LoadThresholdHigh
	}
	return 1000
}

func getSMBCreditsLoadThresholdLow(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.LoadThresholdLow != nil {
		return *dittoServer.Spec.SMB.Credits.LoadThresholdLow
	}
	return 100
}

func getSMBCreditsAggressiveClientThreshold(dittoServer *dittoiov1alpha1.DittoServer) int32 {
	if dittoServer.Spec.SMB != nil && dittoServer.Spec.SMB.Credits != nil && dittoServer.Spec.SMB.Credits.AggressiveClientThreshold != nil {
		return *dittoServer.Spec.SMB.Credits.AggressiveClientThreshold
	}
	return 256
}
