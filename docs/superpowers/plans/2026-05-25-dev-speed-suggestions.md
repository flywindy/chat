# Dev Speed Suggestions Implementation Plan

**Goal:** Reduce iteration time for engineers working on the chat monorepo by scoping the pre-commit hook to changed services and adding a hot-reload dev loop.

**Architecture:** Two independent tooling additions, each gated behind opt-in Makefile targets or hook logic so existing workflows keep working. Pre-commit detects scope from `git diff --cached`; hot-reload uses `air`.

**Tech Stack:** Bash, GNU Make, `air` (Go live-reload).

---

## Included items

### #2 — Scoped pre-commit hook

`.claude/hooks/pre-commit.sh` now:

- Reads `git diff --cached --name-only -- '*.go' 'go.mod' 'go.sum'`.
- Runs `make lint` full-repo (fast enough).
- Decides test scope:
  - `go.mod`/`go.sum` staged → full `./...` (dependency blast radius).
  - Any `pkg/` file staged → full `./...` (shared code blast radius).
  - Only service-dir files → `make test SERVICE=<name>` per affected service.
- Marks success in `.cache/precommit/<hash>` so amend/retry on the same staged set short-circuits. Bypass with `PRECOMMIT_NO_CACHE=1`.

Backing library: `tools/buildcache/buildcache.sh` provides `buildcache_key`/`hit`/`mark` and is sourced by the hook. Smoke-tested via `tools/buildcache/buildcache_test.sh`.

### #3 — `make dev SERVICE=<name>`

Hot-reload one service against the shared deps stack:

- `make tools` installs `air@v1.62.0` alongside the other Go tools.
- `make dev SERVICE=<name>` runs `tools/dev/dev.sh <name>`, which substitutes the service name into `tools/dev/air-template.toml` and execs `air`.
- Watches `<service>/` and `pkg/`, excludes `_test.go` and generated `mock_*.go`.
- Refuses to start if `chat-local-nats` isn't running (clear "run `make deps-up` first" message).

---

## Verification commands

```bash
# Pre-commit cache + scope (touch one service, commit twice)
echo "// noop" >> auth-service/handler.go
git add auth-service/handler.go
git commit -m "test"   # runs make lint + make test SERVICE=auth-service
git commit --amend     # cached: skips both

# Hot-reload (requires `make tools` + `make deps-up`)
make dev SERVICE=broadcast-worker
# edit broadcast-worker/handler.go in another terminal; air rebuilds in ~1s
```

---

## Out of scope (deferred)

- `make sast` caching — minimal Claude-loop benefit, not worth the carrying cost.
- Worker scaffold generator (`make scaffold`) — modest win, can be added later if new services become common.
