# Local Observability — Container CPU & Memory

Run cAdvisor + Prometheus + Grafana locally to observe container-level CPU,
memory, and network trends for every service and dependency in the local dev
environment. Useful when load-testing the messaging pipeline or hunting a hot
service.

## Prerequisites

- `make deps-up` must have run first — this creates the `chat-local` Docker
  network that the observability stack joins.

## Quick Start

```bash
make deps-up     # if not already running
make up          # if you want to see service containers in the dashboard
make obs-up
```

Then open Grafana: **http://localhost:3001**

The dashboard **"Containers - CPU & Memory"** is pre-loaded and refreshes every
5 seconds. Anonymous Admin is enabled, so no login is required.

## Stop

```bash
make obs-down
```

Storage is ephemeral — Prometheus metrics are lost on teardown. UI-made
dashboard edits are blocked by the provisioning config (`allowUiUpdates: false`);
edit `grafana/dashboards/containers-cpu.json` directly to change a dashboard,
then run `make obs-down && make obs-up` to reload.

## Ports

| Port  | Service     | Notes                                                       |
|-------|-------------|-------------------------------------------------------------|
| 3001  | Grafana UI  | `:3001` avoids collision with the frontend dev server :3000 |
| 9091  | Prometheus  | `:9091` avoids collision with `search-service`'s :9090      |
| 8088  | cAdvisor    | `:8088` avoids collision with `auth-service`'s :8080        |

## What's instrumented

cAdvisor reads Docker stats for **every container on the host**, so this works
uniformly for Go services and third-party dependencies (NATS, MongoDB,
Cassandra, Elasticsearch, Valkey, Keycloak, MinIO) — no per-service code
changes required.

Legends show the 12-character container short-ID (e.g. `0a1b2c3d4e5f`) rather
than the container name. cAdvisor populates the `name` label only when its
Docker daemon integration works, which is unreliable on older cAdvisor
versions and on cgroups v2 + systemd hosts. The short-ID is extracted directly
from the cgroup path via `label_replace`, which works on every cAdvisor build.
Cross-reference with `docker ps --format 'table {{.ID}}\t{{.Names}}'` to map
ID → name.

Application-level Go metrics (goroutines, GC, heap) are out of scope here.
When a service exposes `/metrics`, add a scrape job to `prometheus/prometheus.yml`.

## Files

| File | Purpose |
|------|---------|
| `docker-compose.yml` | The three-container stack definition. |
| `prometheus/prometheus.yml` | Prometheus scrape config. |
| `grafana/provisioning/datasources/prometheus.yml` | Auto-wires the Prometheus datasource. |
| `grafana/provisioning/dashboards/dashboards.yml` | Tells Grafana to load every JSON file in `./dashboards`. |
| `grafana/dashboards/containers-cpu.json` | The pre-built dashboard. |

## Scope

**Local development only.** The cAdvisor mount posture (privileged container
with the Docker socket and `/`, `/sys`, `/var/lib/docker` bind-mounted read-only)
grants broad host visibility and is unsuitable for shared or production
environments. All three ports are bound to `127.0.0.1` so the anonymous-admin
Grafana and unauthenticated Prometheus/cAdvisor APIs stay off any LAN the host
is attached to; remove the prefix in `docker-compose.yml` only if you have a
reason to reach the stack from another machine.
