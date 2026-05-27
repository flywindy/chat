#!/bin/bash
# Pre-commit hook for Claude Code. Runs make lint + scoped make test.
#
# Test scoping rules:
#   - go.mod / go.sum staged       → full ./... (dependency change blast radius)
#   - pkg/ file staged             → full ./... (shared code blast radius)
#   - only service dir(s) staged   → just those services
#
# A buildcache marker keyed on the staged file hashes prevents re-running
# when the same set was already verified (useful for amend / no-op retries).
# Bypass with PRECOMMIT_NO_CACHE=1.

set -euo pipefail

INPUT=$(cat)
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty')

if ! echo "$COMMAND" | grep -qE '\bgit\s+commit\b'; then
  exit 0
fi

REPO_ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"
cd "$REPO_ROOT"

export PATH="${HOME}/go/bin:${PATH}"

STAGED_GO=$(git diff --cached --name-only --diff-filter=ACMR -- '*.go' 'go.mod' 'go.sum')

if [ -z "$STAGED_GO" ]; then
  echo "Pre-commit: only non-Go files staged, skipping lint and tests." >&2
  exit 0
fi

KEY=""
if [ "${PRECOMMIT_NO_CACHE:-0}" != "1" ] && [ -f tools/buildcache/buildcache.sh ]; then
  # shellcheck disable=SC1091
  source tools/buildcache/buildcache.sh
  KEY=$(buildcache_key $STAGED_GO)
  if buildcache_hit .cache precommit "$KEY"; then
    echo "Pre-commit: cached (key $KEY); bypass with PRECOMMIT_NO_CACHE=1." >&2
    exit 0
  fi
fi

SCOPE_FULL=0
echo "$STAGED_GO" | grep -qE '^(go\.mod|go\.sum)$' && SCOPE_FULL=1
echo "$STAGED_GO" | grep -qE '^pkg/' && SCOPE_FULL=1

SERVICES=$(echo "$STAGED_GO" \
  | awk -F/ 'NF>1 && $1!="pkg" && $1!="tools" && $1!="docs" && $1!=".github" && $1!="docker-local" && $1!=".claude" && $1!="chat-frontend" {print $1}' \
  | sort -u)

if [ -z "$SERVICES" ] && [ "$SCOPE_FULL" -eq 0 ]; then
  SCOPE_FULL=1
fi

echo "Pre-commit: running make lint..." >&2
if ! make lint >&2 2>&1; then
  echo "BLOCKED: make lint failed. Fix lint errors before committing." >&2
  exit 2
fi

if [ "$SCOPE_FULL" -eq 1 ]; then
  echo "Pre-commit: running make test (full repo)..." >&2
  if ! make test >&2 2>&1; then
    echo "BLOCKED: make test failed. Fix test failures before committing." >&2
    exit 2
  fi
else
  for svc in $SERVICES; do
    echo "Pre-commit: running make test SERVICE=$svc..." >&2
    if ! make test SERVICE="$svc" >&2 2>&1; then
      echo "BLOCKED: make test SERVICE=$svc failed. Fix test failures before committing." >&2
      exit 2
    fi
  done
fi

if [ -n "$KEY" ] && declare -F buildcache_mark >/dev/null; then
  buildcache_mark .cache precommit "$KEY"
fi

echo "Lint and tests passed." >&2
exit 0
