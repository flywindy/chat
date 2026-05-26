# Local Prometheus + Grafana for Container CPU Observability — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a standalone `tools/observability/` docker-compose stack (cAdvisor + Prometheus + Grafana) that surfaces container-level CPU/memory/network trends for every service and dependency in the local dev environment, via a pre-provisioned dashboard.

**Architecture:** Three containers join the existing external `chat-local` Docker network. cAdvisor reads Docker stats from host bind mounts and exposes them on `/metrics`. Prometheus scrapes cAdvisor every 5s. Grafana is launched with anonymous Admin access and provisioned with a Prometheus datasource and one container-CPU dashboard. Storage is ephemeral (no volumes). Stack is opt-in via `make obs-up` / `make obs-down`, mirroring the existing `tools/jaeger/` pattern.

**Tech Stack:** docker-compose, cAdvisor `v0.49.1`, Prometheus `v2.55.1`, Grafana `v11.3.0`, Make.

**Spec:** `docs/superpowers/specs/2026-05-26-local-prometheus-grafana-cpu-observability-design.md`

---

## File Structure

| File | Responsibility |
|------|----------------|
| `tools/observability/docker-compose.yml` | Defines the three containers, mounts, port mappings, and joins the `chat-local` network. |
| `tools/observability/prometheus/prometheus.yml` | Prometheus scrape config — one job (`cadvisor`) initially. |
| `tools/observability/grafana/provisioning/datasources/prometheus.yml` | Auto-wires the Prometheus datasource for Grafana, with a fixed `uid` referenced by the dashboard. |
| `tools/observability/grafana/provisioning/dashboards/dashboards.yml` | Grafana dashboard-provider config — points at `/var/lib/grafana/dashboards`. |
| `tools/observability/grafana/dashboards/containers-cpu.json` | The pre-built "Containers — CPU & Memory" dashboard. |
| `tools/observability/README.md` | One-page usage doc (prerequisites, ports, dashboard URL, scope note). |
| `Makefile` | New `obs-up` / `obs-down` targets, added to `.PHONY`. |

---

### Task 1: Prometheus scrape config and Grafana provisioning files

**Files:**
- Create: `tools/observability/prometheus/prometheus.yml`
- Create: `tools/observability/grafana/provisioning/datasources/prometheus.yml`
- Create: `tools/observability/grafana/provisioning/dashboards/dashboards.yml`

These are static config files. They can't be verified until docker-compose runs in Task 3, so this task ends at "files written and committed."

- [ ] **Step 1: Create the Prometheus scrape config**

Path: `tools/observability/prometheus/prometheus.yml`

```yaml
# Local-dev Prometheus config for tools/observability.
# Forward-compatible: add jobs here when more services start exposing /metrics.
global:
  scrape_interval: 5s
  evaluation_interval: 15s

scrape_configs:
  - job_name: cadvisor
    static_configs:
      - targets: ['cadvisor:8080']
```

- [ ] **Step 2: Create the Grafana datasource provisioning file**

Path: `tools/observability/grafana/provisioning/datasources/prometheus.yml`

The `uid: prometheus` is referenced by the dashboard JSON in Task 2; keep them in sync.

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    uid: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: false
```

- [ ] **Step 3: Create the Grafana dashboard-provider config**

Path: `tools/observability/grafana/provisioning/dashboards/dashboards.yml`

```yaml
apiVersion: 1
providers:
  - name: 'default'
    orgId: 1
    folder: ''
    type: file
    disableDeletion: true
    updateIntervalSeconds: 30
    allowUiUpdates: false
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 4: Sanity-check the three files exist and are non-empty**

Run: `for f in tools/observability/prometheus/prometheus.yml tools/observability/grafana/provisioning/datasources/prometheus.yml tools/observability/grafana/provisioning/dashboards/dashboards.yml; do test -s "$f" || { echo "missing or empty: $f"; exit 1; }; done && echo ok`

Expected: prints `ok`. Schema correctness is verified end-to-end in Task 3 when docker mounts and consumes these files.

- [ ] **Step 5: Commit**

```bash
git add tools/observability/prometheus/prometheus.yml \
        tools/observability/grafana/provisioning/datasources/prometheus.yml \
        tools/observability/grafana/provisioning/dashboards/dashboards.yml
git commit -m "tools(observability): add prometheus + grafana provisioning config"
```

---

### Task 2: Pre-built Grafana dashboard JSON

**Files:**
- Create: `tools/observability/grafana/dashboards/containers-cpu.json`

The dashboard has five panels driven by cAdvisor's `container_*` metrics. The `name!=""` filter excludes the empty-name cgroups cAdvisor also reports. `uid: prometheus` matches the datasource provisioned in Task 1.

- [ ] **Step 1: Write the dashboard JSON**

Path: `tools/observability/grafana/dashboards/containers-cpu.json`

```json
{
  "annotations": { "list": [] },
  "editable": true,
  "fiscalYearStartMonth": 0,
  "graphTooltip": 1,
  "id": null,
  "links": [],
  "liveNow": false,
  "panels": [
    {
      "id": 1,
      "type": "timeseries",
      "title": "CPU % by container",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 9, "w": 24, "x": 0, "y": 0 },
      "fieldConfig": {
        "defaults": { "unit": "percent", "custom": { "drawStyle": "line", "lineWidth": 1, "fillOpacity": 10 } },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "right", "calcs": ["mean", "max"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(container_cpu_usage_seconds_total{name!=\"\"}[1m]) * 100",
          "legendFormat": "{{name}}"
        }
      ]
    },
    {
      "id": 2,
      "type": "bargauge",
      "title": "Top 10 CPU consumers (current)",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 9, "w": 12, "x": 0, "y": 9 },
      "fieldConfig": { "defaults": { "unit": "percent", "min": 0 }, "overrides": [] },
      "options": { "orientation": "horizontal", "displayMode": "gradient", "showUnfilled": true },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "topk(10, rate(container_cpu_usage_seconds_total{name!=\"\"}[1m]) * 100)",
          "legendFormat": "{{name}}",
          "instant": true
        }
      ]
    },
    {
      "id": 3,
      "type": "stat",
      "title": "Container count",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 9, "w": 12, "x": 12, "y": 9 },
      "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
      "options": { "reduceOptions": { "calcs": ["lastNotNull"], "fields": "", "values": false }, "colorMode": "value" },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "count(container_last_seen{name!=\"\"})",
          "instant": true
        }
      ]
    },
    {
      "id": 4,
      "type": "timeseries",
      "title": "Memory usage by container",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 9, "w": 24, "x": 0, "y": 18 },
      "fieldConfig": {
        "defaults": { "unit": "bytes", "custom": { "drawStyle": "line", "lineWidth": 1, "fillOpacity": 10 } },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "right", "calcs": ["mean", "max"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "container_memory_usage_bytes{name!=\"\"}",
          "legendFormat": "{{name}}"
        }
      ]
    },
    {
      "id": 5,
      "type": "timeseries",
      "title": "Network RX/TX by container",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 9, "w": 24, "x": 0, "y": 27 },
      "fieldConfig": {
        "defaults": { "unit": "Bps", "custom": { "drawStyle": "line", "lineWidth": 1, "fillOpacity": 10 } },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "right", "calcs": ["mean", "max"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(container_network_receive_bytes_total{name!=\"\"}[1m])",
          "legendFormat": "{{name}} rx"
        },
        {
          "refId": "B",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(container_network_transmit_bytes_total{name!=\"\"}[1m])",
          "legendFormat": "{{name}} tx"
        }
      ]
    }
  ],
  "refresh": "5s",
  "schemaVersion": 39,
  "tags": ["containers", "cpu", "local-dev"],
  "templating": { "list": [] },
  "time": { "from": "now-15m", "to": "now" },
  "timepicker": {},
  "timezone": "",
  "title": "Containers — CPU & Memory",
  "uid": "containers-cpu",
  "version": 1,
  "weekStart": ""
}
```

- [ ] **Step 2: Verify the JSON parses**

Run: `python3 -c "import json; json.load(open('tools/observability/grafana/dashboards/containers-cpu.json')); print('ok')"`

Expected: prints `ok`.

- [ ] **Step 3: Commit**

```bash
git add tools/observability/grafana/dashboards/containers-cpu.json
git commit -m "tools(observability): add Containers — CPU & Memory dashboard"
```

---

### Task 3: docker-compose.yml — bring the stack up end-to-end

**Files:**
- Create: `tools/observability/docker-compose.yml`

This is the integration moment. The compose file references every config file from Tasks 1 and 2; failures in those would surface here.

**Prerequisite for verification:** the `chat-local` Docker network must exist. It's created by `make deps-up`. Ensure it's up before the verification steps below (`make deps-up` if needed).

- [ ] **Step 1: Write the compose file**

Path: `tools/observability/docker-compose.yml`

```yaml
# Local-dev observability stack: cAdvisor + Prometheus + Grafana.
# Opt-in via `make obs-up` / `make obs-down`. Joins the chat-local network
# created by `make deps-up`. Storage is ephemeral — no named volumes.
#
# Local-dev only: the cAdvisor mount posture (Docker socket + host bind mounts)
# is unsuitable for shared or production environments.

name: chat-local-observability

services:
  cadvisor:
    image: gcr.io/cadvisor/cadvisor:v0.49.1
    container_name: chat-local-cadvisor
    privileged: true
    devices:
      - /dev/kmsg
    ports:
      - "8088:8080"   # host :8088 avoids collision with auth-service's :8080
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:ro
      - /sys:/sys:ro
      - /var/lib/docker/:/var/lib/docker:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/healthz"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 5s

  prometheus:
    image: prom/prometheus:v2.55.1
    container_name: chat-local-prometheus
    depends_on:
      cadvisor:
        condition: service_healthy
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.path=/prometheus
    ports:
      - "9091:9090"   # host :9091 avoids collision with search-service's METRICS_ADDR=:9090
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:9090/-/healthy"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 5s

  grafana:
    image: grafana/grafana:11.3.0
    container_name: chat-local-grafana
    depends_on:
      prometheus:
        condition: service_healthy
    ports:
      - "3001:3000"   # host :3001 avoids collision with the frontend dev server's :3000
    environment:
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: "Admin"
      GF_AUTH_DISABLE_LOGIN_FORM: "true"
      GF_USERS_DEFAULT_THEME: "dark"
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:3000/api/health || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 10s

# All three containers join the chat-local network created by `make deps-up`.
# Lets future scrape jobs target service containers by their compose name
# without re-architecting.
networks:
  default:
    name: chat-local
    external: true
```

- [ ] **Step 2: Verify compose file syntax**

Run: `docker compose -f tools/observability/docker-compose.yml config --quiet`

Expected: exits 0 with no output. If it errors, fix the YAML before continuing.

- [ ] **Step 3: Ensure the chat-local network exists**

Run: `docker network inspect chat-local >/dev/null 2>&1 || make deps-up`

Expected: either the network already exists (no-op) or `make deps-up` brings the deps up and creates it.

- [ ] **Step 4: Bring the stack up and wait for healthchecks**

Run: `docker compose -f tools/observability/docker-compose.yml up -d --wait`

Expected: all three containers transition to `healthy`. If `--wait` times out, run `docker compose -f tools/observability/docker-compose.yml ps` and `docker logs <container>` to diagnose.

- [ ] **Step 5: Verify Prometheus is scraping cAdvisor**

Run: `curl -fsS http://localhost:9091/api/v1/targets | python3 -c "import sys,json; t=[x for x in json.load(sys.stdin)['data']['activeTargets'] if x['labels']['job']=='cadvisor']; assert t and t[0]['health']=='up', t; print('cadvisor target: up')"`

Expected: prints `cadvisor target: up`. If the target is `down`, check the scrape error in the JSON output and fix `prometheus.yml`.

- [ ] **Step 6: Verify Grafana datasource was auto-provisioned**

Run: `curl -fsS http://localhost:3001/api/datasources/uid/prometheus | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['type']=='prometheus' and d['url']=='http://prometheus:9090', d; print('datasource: ok')"`

Expected: prints `datasource: ok`. If it 404s, check `grafana/provisioning/datasources/prometheus.yml` and the volume mount in the compose file.

- [ ] **Step 7: Verify the dashboard was auto-loaded**

Run: `curl -fsS "http://localhost:3001/api/search?query=Containers" | python3 -c "import sys,json; r=json.load(sys.stdin); assert any(d['title']=='Containers — CPU & Memory' for d in r), r; print('dashboard: ok')"`

Expected: prints `dashboard: ok`. If the dashboard is missing, check Grafana's logs for provisioning errors: `docker logs chat-local-grafana 2>&1 | grep -i dashboard`.

- [ ] **Step 8: Verify the dashboard actually has data**

Run: `curl -fsS -G --data-urlencode 'query=rate(container_cpu_usage_seconds_total{name!=""}[1m])' http://localhost:9091/api/v1/query | python3 -c "import sys,json; r=json.load(sys.stdin); assert r['data']['result'], r; print('cpu series count:', len(r['data']['result']))"`

Expected: prints e.g. `cpu series count: 8` (any nonzero count — depends on how many containers are running).

- [ ] **Step 9: Tear the stack down**

Run: `docker compose -f tools/observability/docker-compose.yml down`

Expected: all three containers removed.

- [ ] **Step 10: Commit**

```bash
git add tools/observability/docker-compose.yml
git commit -m "tools(observability): add cAdvisor + Prometheus + Grafana compose stack"
```

---

### Task 4: Makefile targets

**Files:**
- Modify: `Makefile:1-2` (extend `.PHONY`) and append `obs-up` / `obs-down` after the `down` target.

- [ ] **Step 1: Add `obs-up` / `obs-down` to `.PHONY`**

Edit `Makefile`. Replace the existing first two lines:

```make
.PHONY: lint fmt test test-integration generate build deps-up deps-down up down \
        tools sast sast-gosec sast-vuln sast-semgrep
```

with:

```make
.PHONY: lint fmt test test-integration generate build deps-up deps-down up down \
        obs-up obs-down tools sast sast-gosec sast-vuln sast-semgrep
```

- [ ] **Step 2: Add an `OBS_COMPOSE` variable**

After the existing `NATS_CONTAINER := chat-local-nats` line (around `Makefile:8`), append:

```make
OBS_COMPOSE      := tools/observability/docker-compose.yml
```

- [ ] **Step 3: Append the `obs-up` and `obs-down` targets**

Find the existing `down:` target block (around `Makefile:124-129`). Immediately after the last line of that block, append:

```make

# --- Local observability targets ----------------------------------------------
# Start cAdvisor + Prometheus + Grafana. Requires `make deps-up` first so the
# chat-local network exists. Dashboard at http://localhost:3001.
obs-up:
	@docker network inspect chat-local >/dev/null 2>&1 || { \
	  echo "chat-local network missing. Run 'make deps-up' first."; exit 1; \
	}
	docker compose -f $(OBS_COMPOSE) up -d --wait

# Stop the observability stack.
obs-down:
	docker compose -f $(OBS_COMPOSE) down
```

- [ ] **Step 4: Verify `make obs-up` brings the stack up**

Assumes deps are already running (from Task 3, or run `make deps-up` first).

Run: `make obs-up`

Expected: exits 0; `docker ps --filter name=chat-local-` shows `cadvisor`, `prometheus`, `grafana` in addition to the deps.

- [ ] **Step 5: Verify the "deps not running" guard**

Run: `make obs-down && make deps-down && make obs-up`

Expected: `make obs-up` exits non-zero with `chat-local network missing. Run 'make deps-up' first.`

- [ ] **Step 6: Restore deps and verify `make obs-down`**

Run: `make deps-up && make obs-up && make obs-down`

Expected: each step exits 0; after `make obs-down`, `docker ps --filter name=chat-local-cadvisor` returns no rows.

- [ ] **Step 7: Commit**

```bash
git add Makefile
git commit -m "make: add obs-up / obs-down targets for tools/observability"
```

---

### Task 5: README.md

**Files:**
- Create: `tools/observability/README.md`

Mirrors `tools/jaeger/README.md` in tone and structure.

- [ ] **Step 1: Write the README**

Path: `tools/observability/README.md`

````markdown
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

The dashboard **"Containers — CPU & Memory"** is pre-loaded and refreshes every
5 seconds. Anonymous Admin is enabled, so no login is required.

## Stop

```bash
make obs-down
```

Storage is ephemeral — Prometheus metrics and any UI-made dashboard edits are
lost on teardown. Dashboard JSON in this directory is git-tracked and reloaded
automatically on the next `make obs-up`.

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
environments.
````

- [ ] **Step 2: Verify the README renders**

Run: `python3 -c "import re; print('ok' if re.search(r'^# Local Observability', open('tools/observability/README.md').read(), re.M) else 'bad')"`

Expected: prints `ok`.

- [ ] **Step 3: Commit**

```bash
git add tools/observability/README.md
git commit -m "tools(observability): add README"
```

---

### Task 6: End-to-end smoke test

This is a final manual verification that ties everything together. No new files.

- [ ] **Step 1: Bring the full stack up**

```bash
make deps-up && make obs-up
```

Expected: both exit 0.

- [ ] **Step 2: Open Grafana and confirm the dashboard**

Open http://localhost:3001 in a browser. Navigate to **Dashboards → Containers — CPU & Memory**.

Expected:
- Dashboard loads without "datasource not found" errors.
- "CPU % by container" panel shows ≥1 series within ~10s.
- "Top 10 CPU consumers" populates within ~10s.
- "Container count" shows a number ≥ 7 (the dep containers).

If any panel shows "No data", run `docker logs chat-local-prometheus 2>&1 | tail -50` and `docker logs chat-local-grafana 2>&1 | tail -50` to diagnose.

- [ ] **Step 3: Confirm service containers also appear (optional)**

In a second terminal:

```bash
make up   # foreground; Ctrl-C to stop later
```

Wait ~30s for services to register, then refresh the Grafana dashboard.

Expected: container names like `chat-local-broadcast-worker`, `chat-local-message-worker`, etc. appear in the legend alongside the deps (`chat-local-nats`, `chat-local-cassandra`, `chat-local-mongodb`, …).

- [ ] **Step 4: Tear everything down**

```bash
make obs-down && make down && make deps-down
```

Expected: each exits 0.

- [ ] **Step 5: No commit**

This task is verification only — no files changed.

---

## Notes on TDD applicability

This plan is infrastructure / dev tooling: no Go code, no unit tests. CLAUDE.md's TDD rule applies to "all new code" — the verification model here is the runtime smoke check baked into each task (Tasks 3, 4, 6). If a follow-up plan adds Go code (e.g. a `/metrics` listener to a service), that work follows Red-Green-Refactor.
