package config

import (
	"fmt"

	dittoiov1alpha1 "github.com/marmos91/dittofs/dittofs-operator/api/v1alpha1"

	"gopkg.in/yaml.v3"
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
					Bucket:   getConfigValue(backend.Config, "bucket", ""),
					Region:   getConfigValue(backend.Config, "region", "us-east-1"),
					Endpoint: getConfigValue(backend.Config, "endpoint", ""),
				},
			}
		default:
			return "", fmt.Errorf("unsupported backend type %q for backend %q", backend.Type, backend.Name)
		}
	}

	shares := make([]ShareYAML, 0, len(dittoServer.Spec.Config.Shares))
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

		shareYAML := ShareYAML{
			Name:                    share.ExportPath,
			MetadataStore:           share.MetadataStore,
			ContentStore:            share.ContentStore,
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
			Global: map[string]interface{}{},
			Stores: contentStores,
		},
		Shares: shares,
		Adapters: AdaptersConfig{
			NFS: NFSAdapter{
				Enabled:        true,
				Port:           2049,
				MaxConnections: 0,
				Timeouts: TimeoutsConfig{
					Read:     "5m0s",
					Write:    "30s",
					Idle:     "5m0s",
					Shutdown: "30s",
				},
				MetricsLogInterval: "5m0s",
			},
		},
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
