#!/usr/bin/env bash
# Tiny filesystem-based "did we already verify this?" cache. Used by the
# pre-commit hook to skip re-running lint+test when the same staged set has
# already been verified.
#
# Layout: <cache_dir>/<bucket>/<key>  (empty marker file; presence == hit)
#
# This file is sourced as a library — no top-level `set` so caller shell state
# is untouched. Each function uses `local` and guards its own errors.

# buildcache_key <file...> — print a stable hex key for the given files.
# Hashes the SHA-256 of each file's contents, concatenated, then SHA-256s
# the concatenation. Order-independent: inputs are sorted first.
buildcache_key() {
  local f
  local sums=""
  for f in "$@"; do
    if [ -f "$f" ]; then
      sums+=$(sha256sum "$f" | awk '{print $1}')$'\n'
    else
      sums+="missing:$f"$'\n'
    fi
  done
  printf '%s' "$sums" | LC_ALL=C sort | sha256sum | awk '{print $1}'
}

# buildcache_hit <cache_dir> <bucket> <key> — exit 0 if marker exists, 1 if not.
buildcache_hit() {
  local cache_dir="$1" bucket="$2" key="$3"
  test -f "$cache_dir/$bucket/$key"
}

# buildcache_mark <cache_dir> <bucket> <key> — create the marker.
buildcache_mark() {
  local cache_dir="$1" bucket="$2" key="$3"
  mkdir -p "$cache_dir/$bucket"
  : > "$cache_dir/$bucket/$key"
}
