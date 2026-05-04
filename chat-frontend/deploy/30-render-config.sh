#!/bin/sh
# Renders /config.js from env vars at container start.
set -eu

: "${AUTH_URL:=http://localhost:8080}"
: "${NATS_URL:=ws://localhost:9222}"
: "${DEFAULT_SITE_ID:=site-local}"
: "${DEV_MODE:=false}"
: "${OIDC_ISSUER_URL:=}"
: "${OIDC_CLIENT_ID:=nats-chat}"
export AUTH_URL NATS_URL DEFAULT_SITE_ID DEV_MODE OIDC_ISSUER_URL OIDC_CLIENT_ID

envsubst '${AUTH_URL} ${NATS_URL} ${DEFAULT_SITE_ID} ${DEV_MODE} ${OIDC_ISSUER_URL} ${OIDC_CLIENT_ID}' \
  < /etc/config.js.template \
  > /usr/share/nginx/html/config.js

echo "rendered /config.js  AUTH_URL=$AUTH_URL  NATS_URL=$NATS_URL  DEFAULT_SITE_ID=$DEFAULT_SITE_ID  DEV_MODE=$DEV_MODE  OIDC_ISSUER_URL=$OIDC_ISSUER_URL  OIDC_CLIENT_ID=$OIDC_CLIENT_ID"
