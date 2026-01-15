# pkg/config

Configuration parsing and validation - transforms YAML/env vars into typed config.

## Key Files

- `config.go` - Main `Config` struct and component configs
- `stores.go` - Factory functions creating stores from config
- `registry.go` - Registry initialization from config
- `defaults.go` - Default values for all configurations
- `init.go` - `dittofs init` file generation

## Named Stores Pattern

```yaml
metadata:
  stores:
    fast-meta: { type: memory }
    persistent-meta: { type: badger, badger: { db_path: /data } }

content:
  stores:
    s3-content: { type: s3, s3: { bucket: my-bucket } }

shares:
  - name: /archive
    metadata: persistent-meta   # reference by name
    content_store: s3-content
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

Only matching type's nested config is used:
```yaml
content:
  stores:
    mystore:
      type: s3           # ← this determines which nested config applies
      s3: { ... }        # ← used
      filesystem: { ... } # ← ignored
```

## Common Mistakes

1. **Store name typo in share** - silent failure, store not found at runtime
2. **Forgetting defaults** - `defaults.go` fills missing values before validation
3. **Environment case** - must be uppercase with underscores
4. **NTHash in config** - chmod 600, highly sensitive (pass-the-hash risk)
