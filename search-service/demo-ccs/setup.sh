#!/bin/sh
# Demo-CCS one-shot setup (runs inside the `setup` compose service).
#
# Preconditions:
#   - `make deps-up` is up — so `elasticsearch` (host of chat-local)
#     resolves inside the chat-local docker network.
#   - `es-remote` is healthy (compose depends_on guarantees this).
#
# Steps:
#   1. Configure the deps `elasticsearch` as a CCS client of `es-remote`
#      via proxy-mode settings (single TCP conn to es-remote:9300, no
#      node discovery — matches what prod does when clusters sit behind
#      k8s ingress gateways).
#   2. Wait until /_remote/info reports connected=true — the settings
#      PUT returns immediately but the transport handshake is async.
#   3. Upload the same `messages-*` index template to both clusters so
#      `messages-YYYY-MM` indices inherit consistent mappings regardless
#      of which cluster they land on.
#   4. Bulk-index seed messages: room1 on local, room2 on remote.
#   5. Seed the user-room access-control doc on local for alice so
#      search-service's terms-lookup has something to return.
#
# Idempotent: re-running is safe — settings and bulk indexing both
# upsert by _id.

set -eu

ES_LOCAL="http://elasticsearch:9200"
ES_REMOTE="http://es-remote:9200"

echo "==> waiting for deps elasticsearch (from make deps-up)"
local_ready=""
for i in $(seq 1 30); do
  if curl -fsS "$ES_LOCAL/_cluster/health" >/dev/null 2>&1; then
    echo "    local ES reachable after ${i}s"
    local_ready=1
    break
  fi
  sleep 1
done
if [ -z "$local_ready" ]; then
  echo "!! deps elasticsearch at $ES_LOCAL not reachable after 30s."
  echo "   Run \`make deps-up\` first — this demo piggybacks on the shared chat-local network."
  exit 1
fi

echo "==> configuring CCS on local: proxy → es-remote:9300"
curl -sS -X PUT -H "Content-Type: application/json" "$ES_LOCAL/_cluster/settings" -d '{
  "persistent": {
    "cluster.remote.remote1.mode":          "proxy",
    "cluster.remote.remote1.proxy_address": "es-remote:9300",
    "cluster.remote.remote1.skip_unavailable": true
  }
}' >/dev/null

echo "==> waiting for remote1 to report connected=true"
connected=""
for i in $(seq 1 20); do
  connected=$(curl -sS "$ES_LOCAL/_remote/info" | grep -o '"connected":true' || true)
  if [ -n "$connected" ]; then
    echo "    connected after ${i}s"
    break
  fi
  sleep 1
done
if [ -z "$connected" ]; then
  echo "!! remote1 never connected; aborting"
  curl -sS "$ES_LOCAL/_remote/info"
  exit 1
fi

echo "==> uploading messages-* template to both clusters"
# template.json mirrors the mappings + custom_analyzer produced by
# messageTemplateBody() in search-sync-worker/messages.go. If you add a
# field to MessageSearchIndex there, update template.json here too.
for ES in "$ES_LOCAL" "$ES_REMOTE"; do
  curl -sS -X PUT -H "Content-Type: application/json" \
    "$ES/_index_template/messages-demo-template" \
    --data-binary @/template.json >/dev/null
done

echo "==> bulk-indexing room1 messages into local"
curl -sS -X POST -H "Content-Type: application/x-ndjson" \
  "$ES_LOCAL/_bulk?refresh=true" --data-binary @/seed-local.ndjson >/dev/null

echo "==> bulk-indexing room2 messages into remote"
curl -sS -X POST -H "Content-Type: application/x-ndjson" \
  "$ES_REMOTE/_bulk?refresh=true" --data-binary @/seed-remote.ndjson >/dev/null

echo "==> seeding user-room doc for alice on local (rooms: [room1, room2])"
curl -sS -X PUT -H "Content-Type: application/json" \
  "$ES_LOCAL/user-room/_doc/alice?refresh=true" \
  --data-binary @/user-room.json >/dev/null

echo ""
echo "==> demo ready"
echo "    Kibana:         http://localhost:5601  (Dev Tools → Console)"
echo "    Local ES:       http://localhost:9200  (from deps)"
echo "    Remote ES:      http://localhost:9201  (from this demo)"
echo ""
echo "    Kibana quick test:"
echo "      GET messages-*,*:messages-*/_search"
echo "      { \"query\": { \"match\": { \"content\": \"hello\" } } }"
echo ""
echo "    NATS quick test (requires \`nats\` CLI on host):"
echo "      nats req chat.user.alice.request.search.messages \\"
echo "        '{\"searchText\":\"hello\"}' \\"
echo "        --creds docker-local/backend.creds \\"
echo "        --server nats://localhost:4222"
