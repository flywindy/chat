#!/bin/bash
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"

# --- Synchronous: plugin install + PATH persist (seconds) ---
# Blocks the session start so superpowers skills are available immediately
# and so $CLAUDE_ENV_FILE is written before the session snapshots its env.

claude plugin marketplace add obra/superpowers-marketplace 2>/dev/null || true
claude plugin install superpowers@superpowers-marketplace --scope project

GOBIN_DIR="$(go env GOPATH)/bin"
export PATH="${GOBIN_DIR}:${PATH}"
if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  echo "export PATH=\"${GOBIN_DIR}:\${PATH}\"" >> "$CLAUDE_ENV_FILE"
fi

# --- Background: Go tool compilation + module download (minutes) ---
# Detached so the session is live the moment the synchronous block above
# finishes. Output goes to a log file since the parent's stdout/stderr
# close on hook exit. tail -f the log to watch progress.
LOG="/tmp/claude-session-start-bg.log"
nohup bash -c '
  set -euo pipefail
  command -v pipx >/dev/null 2>&1 || uv tool install pipx
  make tools
  go install go.uber.org/mock/mockgen@v0.6.0
  GOPROXY=direct go mod download github.com/klauspost/compress || true
  GOPROXY=https://proxy.golang.org,direct go mod download
  GOPROXY=https://proxy.golang.org,direct go vet ./... || true
  echo "=== session-start background install complete ==="
' >"$LOG" 2>&1 </dev/null &
disown
