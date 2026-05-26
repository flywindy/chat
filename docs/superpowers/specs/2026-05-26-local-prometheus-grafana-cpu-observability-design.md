# Local Prometheus + Grafana for Container CPU Observability

**Date:** 2026-05-26
**Status:** Approved
**Approach:** Standalone docker-compose tool stack at `tools/observability/` (cAdvisor + Prometheus + Grafana), pre-provisioned container-CPU dashboard, ephemeral storage.

## Summary

Add a self-contained local observability stack that surfaces CPU (and memory / network) trends across every container in the local dev environment — both the 12 microservices and their dependencies (NATS, MongoDB, Cassandra, Elasticsearch, Valkey, Keycloak, MinIO). Container-level metrics come from a single cAdvisor instance, scraped by Prometheus and visualized through a pre-provisioned Grafana dashboard. No application code changes; works uniformly for Go services and third-party deps.

The stack is opt-in via `make obs-up` / `make obs-down`, mirroring the existing `tools/jaeger/` pattern. It joins the existing `chat-local` Docker network so any future application-level scrape jobs can target services by their compose service name.

## Decision

| Decision | Choice |
|----------|--------|
| Location | `tools/observability/` (mirrors `tools/jaeger/`) |
| Metrics source | cAdvisor reading Docker stats — covers every container, no per-service instrumentation |
| Network | Joins existing external `chat-local` network |
| Storage | Ephemeral — no Docker volumes; data lost on `make obs-down` |
| Dashboard | One pre-provisioned dashboard: "Containers — CPU & Memory" |
| Lifecycle | Opt-in (`make obs-up` / `make obs-down`); not started by `make deps-up` |
| App-level metrics | Out of scope for this design — Prometheus config is forward-compatible so jobs can be added later without re-architecting |

## Architecture

Three containers, all on the `chat-local` external network:

| Container | Image | Host port | Purpose |
|-----------|-------|-----------|---------|
| cAdvisor  | `gcr.io/cadvisor/cadvisor:v0.49.1` | `8088` | Reads Docker stats (`/var/lib/docker`, `/sys`, `/var/run/docker.sock`); exposes `/metrics` on container port `8080`. Host port `:8088` chosen to avoid collision with `auth-service`'s `8080:8080` mapping. |
| Prometheus | `prom/prometheus:v2.55.1` | `9091` | Scrapes cAdvisor every 5s. Host port `:9091` chosen to avoid collision with `search-service`'s `METRICS_ADDR=:9090`. Storage is ephemeral (no volume); effective retention is "until `make obs-down`". |
| Grafana   | `grafana/grafana:11.3.0` | `3001` | UI for the pre-provisioned dashboard. `:3001` chosen to avoid collision with the frontend dev server on `:3000`. Anonymous Admin enabled for local-only zero-friction access. |

All three use `tmpfs` (or no volume) for their data dirs, so every restart starts fresh.

cAdvisor requires three host bind mounts to enumerate containers and read cgroup stats:

| Host path | Container path | Mode |
|-----------|----------------|------|
| `/`                  | `/rootfs`           | `ro` |
| `/var/run`           | `/var/run`          | `ro` |
| `/sys`               | `/sys`              | `ro` |
| `/var/lib/docker/`   | `/var/lib/docker`   | `ro` |
| `/var/run/docker.sock` | `/var/run/docker.sock` | `ro` |

This grants cAdvisor effective read access to all containers on the host. Acceptable for local dev only; this stack must not be deployed to shared or production environments.

## File Layout

```
tools/observability/
  docker-compose.yml
  README.md
  prometheus/
    prometheus.yml
  grafana/
    provisioning/
      datasources/
        prometheus.yml
      dashboards/
        dashboards.yml
    dashboards/
      containers-cpu.json
```

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Defines the three containers, mounts, ports, and `chat-local` network join. |
| `README.md` | One-page how-to: prerequisites (`make deps-up` first), `make obs-up` / `make obs-down`, dashboard URL, port table, scope note (local-only). |
| `prometheus/prometheus.yml` | Scrape config — one job (`cadvisor`) initially; structure invites future app jobs. |
| `grafana/provisioning/datasources/prometheus.yml` | Auto-wires the Prometheus datasource. |
| `grafana/provisioning/dashboards/dashboards.yml` | Tells Grafana to load every JSON file in `./dashboards`. |
| `grafana/dashboards/containers-cpu.json` | The pre-built dashboard (committed to git). |

## Prometheus Scrape Config

`prometheus/prometheus.yml`:

```yaml
global:
  scrape_interval: 5s
  evaluation_interval: 15s

scrape_configs:
  - job_name: cadvisor
    static_configs:
      - targets: ['cadvisor:8080']
```

5s interval gives enough resolution to see CPU spikes during a load test without overwhelming Prometheus. Since storage is ephemeral and the container has no volume, effective retention is "until `make obs-down`".

## Grafana Dashboard: "Containers — CPU & Memory"

Built from cAdvisor's `container_*` metrics. The `name!=""` filter excludes the pause / scratch cgroups that cAdvisor also reports.

| Panel | Type | Query |
|-------|------|-------|
| CPU % by container | Time series | `rate(container_cpu_usage_seconds_total{name!=""}[1m]) * 100`, legend `{{name}}` |
| Top 10 CPU consumers (current) | Bar gauge | `topk(10, rate(container_cpu_usage_seconds_total{name!=""}[1m]) * 100)` |
| Memory usage by container | Time series | `container_memory_usage_bytes{name!=""}`, legend `{{name}}` |
| Network RX/TX by container | Time series | `rate(container_network_receive_bytes_total{name!=""}[1m])` and `rate(container_network_transmit_bytes_total{name!=""}[1m])` |
| Container count | Stat | `count(container_last_seen{name!=""})` |

Dashboard settings: refresh = 5s, default time range = last 15m, datasource = the auto-provisioned Prometheus.

Datasource provisioning (`grafana/provisioning/datasources/prometheus.yml`):

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

(Container-internal Prometheus port stays at `9090`; only the host-side port is remapped to `9091`.)

Anonymous-Admin Grafana env (`docker-compose.yml`):

```yaml
GF_AUTH_ANONYMOUS_ENABLED: "true"
GF_AUTH_ANONYMOUS_ORG_ROLE: "Admin"
GF_AUTH_DISABLE_LOGIN_FORM: "true"
```

Local-dev only; same posture as the existing Jaeger UI.

## Makefile Integration

Two new targets appended to the root `Makefile`, modeled on the existing `deps-up` / `deps-down` and following the same `--wait` and self-documenting `##` comment style:

```make
obs-up:    ## Start Prometheus + Grafana + cAdvisor (observability stack)
	docker compose -f tools/observability/docker-compose.yml up -d --wait

obs-down:  ## Stop the observability stack
	docker compose -f tools/observability/docker-compose.yml down
```

Prerequisite documented in `tools/observability/README.md`: `make deps-up` must have run first so the `chat-local` network exists.

## Usage Flow

```
make deps-up        # NATS, Mongo, Cassandra, ES, ...  (creates chat-local network)
make up             # 12 microservices
make obs-up         # cAdvisor + Prometheus + Grafana
# open http://localhost:3001
# dashboard "Containers — CPU & Memory" auto-loads with live data
# run load test, observe CPU trends
make obs-down       # tear down (data discarded)
```

## Verification

Manual smoke test only — this is dev tooling, not application code, so no Go unit or integration tests are added.

1. `make deps-up && make up && make obs-up`
2. `curl -s http://localhost:9091/api/v1/targets | jq '.data.activeTargets[] | {job:.labels.job, health:.health}'` → cAdvisor target is `up`.
3. Open `http://localhost:3001` → dashboard "Containers — CPU & Memory" is present and shows ≥1 series per panel.
4. Optional: drive load with `tools/loadgen` and confirm CPU spikes appear on `chat-local-broadcast-worker`, `chat-local-cassandra`, etc.

## Out of Scope (and forward-compatibility notes)

- **App-level metrics scraping.** Only `search-service` currently exposes `/metrics`. Adding a `services` scrape job in `prometheus.yml` later is a config-only change; no architectural rework needed.
- **Per-service `/metrics` listeners on the other 11 services.** Tracked separately; not blocked by this design.
- **Persistent storage / cross-session comparison.** Switch to named volumes when needed — single-file change.
- **Alerting / Alertmanager.** No alerting in local dev.
- **Production deployment.** This stack is local-only; the cAdvisor mount posture is unsuitable for shared environments.
