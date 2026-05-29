# bench/adapters (stub)

Status: **not yet implemented**.

## Scope

Benchmark protocol adapter framing without a real network: NFS XDR
encode/decode and SMB2/3 wire serialization. Establishes a baseline
for the `bench/e2e` real-client runs by isolating CPU cost spent on
the wire layer from filesystem-engine cost.

## Intended workloads

- `nfs-xdr-encode` — encode a populated `GETATTR` / `READDIRPLUS` reply
- `nfs-xdr-decode` — decode a captured `WRITE` request stream
- `smb2-create-frame` — full SMB2 CREATE request build + sign + verify
- `smb3-encrypt` — AES-CCM / AES-GCM payload encrypt vs message size
- `smb-compound` — compound request encode + dispatch overhead

## Library layout (when wired)

```
bench/adapters/
  doc.go
  nfs.go         // XDR-only workloads
  smb.go         // framing + sign + encrypt workloads
  workloads.go   // RunWorkload(ctx, opts)
  workloads_test.go
```

## Running (once implemented)

```sh
./dfsbench adapters --workload nfs-xdr-encode --ops 100000
./dfsbench adapters --workload smb3-encrypt   --block-size 1048576
```

## Tracking

Not yet filed.
