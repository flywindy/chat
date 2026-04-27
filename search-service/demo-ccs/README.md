# Cross-Cluster Search Demo

Stands up a **second** Elasticsearch cluster alongside your existing
local dev deps, wires it to the deps ES via CCS proxy mode, and seeds
two rooms (one per cluster) so you can see a single query fan out.
Exercise it either from **Kibana Dev Tools** or via the **real
`search-service` + `nats` CLI** — both hit the same wiring.

## What's here

| File | Role |
|---|---|
| `compose.ccs.yaml` | Adds ONE service, `es-remote`, to the shared `chat-local` docker network. Plus a one-shot `setup` service (profile `setup`). No second NATS / Valkey / Kibana — all reused from `make deps-up`. |
| `setup.sh` | Configures CCS on the deps `elasticsearch` → `es-remote:9300` in **proxy mode**, uploads the `messages-*` template to both, bulk-indexes room1 / room2 data, seeds a user-room doc for `alice`. |
| `template.json` | `messages-*` template mirroring prod (custom_analyzer, same 10 fields). Priority `10` so a real prod/sync-worker template with a more specific pattern takes precedence. |
| `seed-local.ndjson` | 4 messages in `room1`, indexed into the deps ES. |
| `seed-remote.ndjson` | 4 messages in `room2`, indexed into `es-remote`. |
| `user-room.json` | Alice's access-control doc — lives on the deps ES only, lists both rooms in `rooms[]`. |

## How to run

```bash
# 1. Bring up shared deps (NATS + ES + Valkey + Kibana + Mongo + Cassandra).
#    One-time per machine — if they're already running, skip.
make deps-up

# 2. Bring up search-service in the background (uses backend.creds from docker-local).
docker compose -f search-service/deploy/docker-compose.yml up --build -d

# 3. Add the second ES cluster + run CCS wiring + seed.
docker compose -f search-service/demo-ccs/compose.ccs.yaml up -d --wait
docker compose -f search-service/demo-ccs/compose.ccs.yaml --profile setup run --rm setup
```

## Exercise it — Kibana Dev Tools

Open http://localhost:5601 → left sidebar → **Dev Tools** → Console:

```
# 1. Confirm CCS is wired
GET _remote/info

# 2. Local-only search (just the deps ES messages-*)
GET messages-*/_search
{ "query": { "match": { "content": "hello" } } }

# 3. CCS search: fans out to es-remote as well
GET messages-*,*:messages-*/_search
{ "query": { "match": { "content": "hello" } } }

# 4. Same query scoped to a specific room on the remote cluster
GET remote1:messages-*/_search
{ "query": { "term": { "roomId": "room2" } } }
```

`_index` on each hit tells you which cluster it came from:
- `"_index": "messages-2026-04"` → local
- `"_index": "remote1:messages-2026-04"` → remote via CCS

### The full `search-service` query for alice

This is what `search-service` actually sends for a global (non-scoped)
`searchText=hello` request — recent-window filter + terms-lookup
against alice's user-room doc + bool_prefix multi_match + pagination:

```
GET messages-*,*:messages-*/_search?ignore_unavailable=true&allow_no_indices=true
{
  "from": 0,
  "size": 25,
  "track_total_hits": true,
  "query": {
    "bool": {
      "must":   [{ "multi_match": { "query": "hello", "type": "bool_prefix", "operator": "AND", "fields": ["content"] } }],
      "filter": [
        { "range": { "createdAt": { "gte": "now-8760h" } } },
        { "bool": {
            "should": [{ "terms": { "roomId": { "index": "user-room", "id": "alice", "path": "rooms" } } }],
            "minimum_should_match": 1
        }}
      ]
    }
  },
  "sort": [ "_score", { "createdAt": "desc" } ]
}
```

The `terms-lookup` pulls alice's allowed rooms from the user-room doc
on the local cluster and filters hits across BOTH clusters. Note that
`user-room` lives only on the local cluster — CCS intentionally does
not forward terms-lookup to the remote; the filter is applied locally
to results merged from all clusters.

## Exercise it — real `search-service` via NATS CLI

If `make up SERVICE=search-service` is running, the service is already
subscribed to `chat.user.*.request.search.messages` inside the
`chat-local` network. NATS is exposed on `localhost:4222` and the
shared `backend.creds` from `docker-local/` grants `pub >` / `sub >`.

Install the `nats` CLI if you don't have it:
```bash
go install github.com/nats-io/natscli/cmd/nats@latest
# or: brew install nats-io/nats-tools/nats
```

Then from the repo root:

```bash
nats req chat.user.alice.request.search.messages \
  '{"searchText":"hello","size":10}' \
  --creds docker-local/backend.creds \
  --server nats://localhost:4222
```

You should see a JSON reply with hits from both clusters. Each hit's
`_index` (if you expand the ES `_search` response) carries the
`remote1:` prefix for docs from the remote cluster — that's how CCS
marks them. The `siteId` field is copied from what was *seeded*
into each cluster, so `site-local` / `site-remote` also happen to
line up here, but the index prefix is the authoritative "which
cluster" signal. No `docker exec` needed — the NATS broker routes
the request to the service's queue group subscriber inside the
container.

### Variations

Scope to a specific room:
```bash
nats req chat.user.alice.request.search.messages \
  '{"searchText":"hello","roomIds":["room2"]}' \
  --creds docker-local/backend.creds --server nats://localhost:4222
```

Confirm access control — bob has no user-room doc, so the filter
returns zero allowed rooms:
```bash
nats req chat.user.bob.request.search.messages \
  '{"searchText":"hello"}' \
  --creds docker-local/backend.creds --server nats://localhost:4222
# → {"total":0,"results":[]}
```

## Tear down

```bash
# Just the demo add-on — drops es-remote + its volume:
docker compose -f search-service/demo-ccs/compose.ccs.yaml down -v

# The `cluster.remote.remote1.*` persistent settings live on the DEPS
# elasticsearch and survive `down -v`. Scrub them if you want a clean
# slate (otherwise they're harmless — the remote just reports
# disconnected until you bring es-remote back up):
curl -s -X PUT -H 'Content-Type: application/json' \
  http://localhost:9200/_cluster/settings \
  -d '{"persistent":{"cluster.remote.remote1":null}}'

# Or nuke the whole stack (recreating the ES volume clears them too):
make down
make deps-down
```

## Why proxy mode (not sniff)

Short version: docker bridge networking mirrors the k8s-with-ingress
prod topology where remote clusters are reachable only via a single
stable address, not as a fleet of node-routable pods. See the spec's
"Production CCS Topology Notes" section for the full decision matrix
(including when sniff is preferable — node-routable pod networking
via Calico BGP / AWS VPC CNI / GKE alias IPs).
