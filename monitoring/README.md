# DittoFS Monitoring Stack

This directory contains a complete monitoring setup for DittoFS using Prometheus and Grafana.

## Quick Start

### 1. Start DittoFS with metrics enabled

Make sure DittoFS is running with the metrics endpoint on port 9090:

```bash
./dfs start  # Metrics exposed on :9090/metrics by default
```

### 2. Start Prometheus and Grafana

```bash
cd monitoring
docker-compose up -d
```

This will start:
- **Prometheus** on http://localhost:9091 (scraping DittoFS metrics every 5s)
- **Grafana** on http://localhost:3000 (admin/admin)

### 3. Access Grafana

1. Open http://localhost:3000
2. Login with `admin` / `admin`
3. The **"DittoFS NFS Performance"** dashboard is automatically loaded

## Dashboard Overview

### DittoFS NFS Performance

Monitor NFS protocol performance and behavior:

**NFS Overview**
- **Active Connections**: Current number of active NFS client connections
- **Request Rate**: Total NFS requests per second
- **Error Rate**: Percentage of failed requests
- **Request Latency (p95)**: 95th percentile request latency

**Request Metrics**
- **Requests per Second by Procedure**: Breakdown of which NFS operations are being called (READ, WRITE, LOOKUP, etc.)
- **Request Duration by Procedure**: Latency distribution for each operation type
- **Requests In Flight**: Currently processing requests by procedure
- **Requests per Second by Share**: Request rate per NFS export

**Data Transfer**
- **Throughput by Direction**: Read vs write throughput in MB/s
- **Throughput by Procedure**: Which operations are transferring the most data
- **Operation Size Distribution**: Size of READ/WRITE operations (p50, p95)
- **Bytes Transferred by Procedure**: Pie chart showing data transfer distribution

**Connection Metrics**
- **Active Connections Over Time**: Connection count trends
- **Connection Lifecycle**: Rate of connections accepted, closed, and force-closed
- **Total Connection Stats**: Cumulative connection counters

**Error Analysis**
- **Errors per Second by Procedure**: Which operations are failing
- **Error Codes Distribution**: NFS error codes breakdown
- **Top 20 Errors**: Table of most frequent errors by procedure and error code

## Running Benchmarks with Monitoring

1. Start the monitoring stack:
```bash
cd monitoring && docker-compose up -d
```

2. Open Grafana dashboard at http://localhost:3000

3. Mount DittoFS and run operations:
```bash
# Mount NFS share
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049,resvport localhost:/export /mnt/test

# Run some file operations
cd /mnt/test
dd if=/dev/zero of=testfile bs=1M count=100
cat testfile > /dev/null
ls -la
```

4. Watch the metrics in real-time:
   - Request rate shows which operations are being performed
   - Latency graphs show performance characteristics
   - Throughput graphs show data transfer rates
   - Error analysis helps identify issues

## Metrics Available

### NFS Metrics
- `dittofs_nfs_requests_total{procedure,share,status,error_code}` - Total number of NFS requests
  - Labels:
    - `procedure`: NFS procedure name (READ, WRITE, LOOKUP, GETATTR, etc.)
    - `share`: Share/export path
    - `status`: success or error
    - `error_code`: NFS error code (empty for success)
- `dittofs_nfs_request_duration_milliseconds{procedure,share}` - Duration of NFS requests in milliseconds
  - Buckets: 1ms, 10ms, 100ms, 1s, 10s
- `dittofs_nfs_requests_in_flight{procedure,share}` - Current number of NFS requests being processed
- `dittofs_nfs_bytes_transferred_total{procedure,share,direction}` - Total bytes transferred via NFS operations
  - Labels:
    - `direction`: read or write
- `dittofs_nfs_operation_size_bytes{operation,share}` - Distribution of READ/WRITE operation sizes
  - Buckets: 4KB, 64KB, 1MB, 10MB
- `dittofs_nfs_active_connections` - Current number of active NFS connections
- `dittofs_nfs_connections_accepted_total` - Total number of NFS connections accepted
- `dittofs_nfs_connections_closed_total` - Total number of NFS connections closed
- `dittofs_nfs_connections_force_closed_total` - Total number of NFS connections force-closed during shutdown

## Customizing

### Change scrape interval
Edit `prometheus.yml`:
```yaml
global:
  scrape_interval: 5s  # Change to 1s for more granular data
```

### Add alerting
Create `prometheus/alerts.yml` and add alerting rules for:
- High request latency
- High error rates
- Connection saturation

### Create custom dashboards
1. Go to Grafana → Dashboards → New
2. Use the metrics listed above
3. Save and export JSON to `grafana/dashboards/`

## Troubleshooting

### Prometheus can't scrape DittoFS
- Check DittoFS is running: `curl http://localhost:9090/metrics`
- On macOS, Prometheus uses `host.docker.internal` to reach host services
- On Linux, you may need to change prometheus.yml to use your host IP

### No data in Grafana
- Check Prometheus targets: http://localhost:9091/targets
- DittoFS target should show "UP"
- Check Grafana datasource: Settings → Data Sources → Prometheus

### Dashboard doesn't load
- Restart Grafana: `docker-compose restart grafana`
- Check logs: `docker-compose logs grafana`

## Stopping

```bash
docker-compose down         # Stop containers
docker-compose down -v      # Stop and remove data volumes
```

## Architecture

```
┌──────────┐     :9090      ┌────────────┐
│ DittoFS  │────metrics────▶│ Prometheus │
└──────────┘                └──────┬─────┘
                                   │
                                   │ queries
                                   ▼
                            ┌─────────────┐
                            │   Grafana   │
                            └─────────────┘
                                 :3000
```

Prometheus scrapes DittoFS metrics every 5 seconds and stores them in a time-series database. Grafana queries Prometheus to visualize the metrics.

## Distributed Tracing (OpenTelemetry)

DittoFS supports distributed tracing via OpenTelemetry, allowing you to trace requests across NFS operations and storage backends.

### Enabling Tracing

Add telemetry configuration to your `config.yaml`:

```yaml
telemetry:
  enabled: true
  endpoint: "localhost:4317"  # OTLP gRPC endpoint
  insecure: true              # Use for local development
  sample_rate: 1.0            # Sample all traces (reduce in production)
```

Or via environment variables:

```bash
DITTOFS_TELEMETRY_ENABLED=true \
DITTOFS_TELEMETRY_ENDPOINT=localhost:4317 \
DITTOFS_TELEMETRY_INSECURE=true \
./dfs start
```

### Running Jaeger Locally

Add Jaeger to your monitoring stack:

```yaml
# Add to docker-compose.yml
jaeger:
  image: jaegertracing/all-in-one:1.51
  ports:
    - "16686:16686"   # Jaeger UI
    - "4317:4317"     # OTLP gRPC
    - "4318:4318"     # OTLP HTTP
  environment:
    - COLLECTOR_OTLP_ENABLED=true
```

Then access the Jaeger UI at http://localhost:16686

### What's Traced

DittoFS traces include:

- **NFS Operations**: Each NFS procedure (READ, WRITE, LOOKUP, etc.) creates a span
- **Storage Operations**: S3 uploads/downloads, BadgerDB queries, filesystem I/O
- **Cache Operations**: Cache hits, misses, flushes, and prefetch operations
- **Context Propagation**: Client IP, file handles, paths, and operation metadata

### Trace Sampling

In production, sample traces to reduce overhead:

```yaml
telemetry:
  sample_rate: 0.1  # Sample 10% of traces
```

Recommended sampling rates:
- **Development**: `1.0` (all traces)
- **Staging**: `0.5` (50% of traces)
- **Production**: `0.01` to `0.1` (1-10% of traces)
