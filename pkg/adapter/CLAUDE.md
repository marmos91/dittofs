# pkg/adapter

Protocol adapters - network servers that expose filesystem via NFS/SMB protocols.

## Adapter Lifecycle

```
Creation → SetRegistry() → Serve() → Stop()
                              ↓
                         context.Canceled on graceful shutdown
```

- `SetRegistry()`: Called once before `Serve()`, injects shared backend access
- `Serve()`: Blocks until context cancelled or fatal error
- `Stop()`: Safe to call concurrently, idempotent via `sync.Once`

## NFS Adapter (`nfs/`)

### Graceful Shutdown Flow
```
SIGINT/SIGTERM → Cancel context → Close listener →
Wait (up to timeout) → Force close remaining connections
```

### Connection Management
- `sync.Map` for concurrent-safe connection tracking (optimized for high churn)
- `sync.WaitGroup` tracks active connections for shutdown wait
- Optional semaphore limits concurrent connections

### Non-Obvious Details
- `shutdownOnce` ensures idempotent shutdown
- `listenerReady` channel exists for test synchronization
- Stores `writeCache` reference for graceful flush on shutdown

## SMB Adapter (`smb/`)

Similar lifecycle to NFS but with:
- Session state management
- NTLM/SPNEGO authentication
- Different timeout semantics

## Common Mistakes

1. **Calling Serve() before SetRegistry()** - nil pointer panic
2. **Not returning context.Canceled** - caller can't distinguish graceful vs error
3. **Blocking in Stop()** - should trigger shutdown, not wait for it
4. **Ignoring shutdown timeout** - force close after deadline, don't hang forever
