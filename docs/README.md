# Dagger Cache

A self-hosted, Dagger-Cloud-compatible platform that provides remote shared cache, auto-scaling engine fleets, live pipeline UI, and CI integration.

## Quick Start

### Client Setup

```bash
export DAGGER_CLOUD_URL=https://your-supervisor.example.com
export DAGGER_CLOUD_TOKEN=your-token
export _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self
export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.reg/dagger-cache:V0-21-4,mode=max"
# optional: pin version
export _EXPERIMENTAL_DAGGER_TAG=v0.21.4

dagger call github.com/your-org/ci@v1.0.0 build
```

Or use the wrapper:
```bash
./cmd/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
```

### Docker (Dev Mode)

```bash
cd deploy/docker
docker compose up -d
```

This starts the Supervisor, OTel Collector, Tempo, Loki, Prometheus, Grafana, and a local cache registry.

### Kubernetes

```bash
kubectl apply -f deploy/k8s/
```

## Architecture

The Supervisor provides three functions:
1. **Control Plane** (Hertz HTTPS): `POST /v1/engines` provisions engine pods per version
2. **Data Plane** (mTLS L4 proxy): pins client connections to specific engine replica pods
3. **OTLP Ingest**: forwards Dagger CLI telemetry to the telemetry stack

### Engine Fleet

Per-version StatefulSet (e.g., `dagger-engine-v0-21-4`) with auto-scaling, idle scale-down, and version-level TTL.

### Remote Shared Cache

Self-hosted OCI registry (`registry:2`) storing BuildKit cache blobs. Engines push/pull cache layers per solve. Client sets the cache ref via env var.

## CI Integration

### GitHub Actions
```yaml
- uses: ./ci-integrations/gha
  with:
    server-url: https://supv.example.com
    token: ${{ secrets.DAGGER_CLOUD_TOKEN }}
    module: github.com/org/ci@v1.0.0
    args: build
```

### Jenkins
```groovy
@Library('dagger-cache') _
daggerCache(serverUrl: 'https://supv.example.com', token: env.TOKEN) {
  sh 'dagger ...'
}
```

### Drone
```yaml
steps:
  - name: dagger-cache
    image: dagger-cache/drone-config-extension
    settings:
      server_url: https://supv.example.com
      token:
        from_secret: dagger_cache_token
```

## Configuration

See `config.app.yaml` for all options. Environment variable overrides use `DAGGER_CACHE_` prefix.

## Contract Drift Monitoring

The Supervisor mirrors the Dagger cloud contract. Monitor these files in the Dagger source:
- `internal/cloud/client.go` — EngineSpec format
- `engine/telemetry/cloud.go` — OTLP export configuration
- `engine/client/client.go` — cache env var handling
