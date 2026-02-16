# pkg/config

Configuration parsing and validation - transforms YAML/env vars into typed config.

## Key Files

- `config.go` - Main `Config` struct and component configs
- `stores.go` - Factory functions creating stores from config
- `runtime.go` - Control plane runtime initialization from config
- `defaults.go` - Default values for all configurations
- `init.go` - `dittofs init` file generation

## Named Stores Pattern

Stores, shares, and adapters are managed via `dittofsctl` (persisted in the control plane database):

```bash
# Create named stores
./dittofsctl store metadata add --name fast-meta --type memory
./dittofsctl store metadata add --name persistent-meta --type badger \
  --config '{"path":"/data"}'
./dittofsctl store payload add --name s3-payload --type s3 \
  --config '{"bucket":"my-bucket"}'

# Create share referencing stores by name
./dittofsctl share create --name /archive --metadata persistent-meta --payload s3-payload
```

- Stores created once, shared across shares
- Each share references stores by name
- Enables resource efficiency (one S3 client for multiple shares)

## Environment Variable Override

`DITTOFS_*` overrides config file via Viper:
```
DITTOFS_LOGGING_LEVEL=DEBUG
DITTOFS_ADAPTERS_NFS_PORT=3049
DITTOFS_SERVER_SHUTDOWN_TIMEOUT=60s
```

## Type-Specific Nested Config

When using the `--config` JSON flag, only the fields matching the store type are used:
```bash
# The type determines which config fields apply
./dittofsctl store payload add --name mystore --type s3 \
  --config '{"bucket":"my-bucket","region":"us-east-1"}'  # S3-specific fields
```

## Common Mistakes

1. **Store name typo in CLI** - `dittofsctl share create` will fail if the referenced store doesn't exist
2. **Forgetting defaults** - `defaults.go` fills missing values before validation
3. **Environment case** - must be uppercase with underscores
4. **NTHash in config** - chmod 600, highly sensitive (pass-the-hash risk)
