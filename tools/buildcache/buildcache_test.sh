#!/usr/bin/env bash
# Smoke tests for tools/buildcache/buildcache.sh.
# Runs each function and asserts behavior. Exits non-zero on any failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/buildcache.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/a"
echo "hello" > "$tmp/a/x.go"
echo "world" > "$tmp/a/y.go"
k1=$(buildcache_key "$tmp/a/x.go" "$tmp/a/y.go")
k2=$(buildcache_key "$tmp/a/x.go" "$tmp/a/y.go")
[ "$k1" = "$k2" ] || { echo "FAIL: key not stable"; exit 1; }

echo "changed" > "$tmp/a/x.go"
k3=$(buildcache_key "$tmp/a/x.go" "$tmp/a/y.go")
[ "$k1" != "$k3" ] || { echo "FAIL: key did not change after file edit"; exit 1; }

cache_dir="$tmp/cache"
buildcache_hit "$cache_dir" "test-bucket" "$k3" && { echo "FAIL: hit before mark"; exit 1; } || true
buildcache_mark "$cache_dir" "test-bucket" "$k3"
buildcache_hit "$cache_dir" "test-bucket" "$k3" || { echo "FAIL: miss after mark"; exit 1; }

buildcache_hit "$cache_dir" "test-bucket" "different-key" && { echo "FAIL: hit on unrelated key"; exit 1; } || true

echo "OK"
