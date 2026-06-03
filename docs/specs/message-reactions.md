# Spec: Reactions — Embedded `MAP<reaction_key, reactor_info>`

> **Status:** SHIPPED in PR #221 (branch `claude/modest-mccarthy-KarZE`) as Commit A `ca34108` (revert v2) + Commit B `7e9c5fb` (introduce v3), plus follow-up cleanup commits. This document is preserved as the design record; the forward-looking language ("Commit A", "Commit B", "the implementer must …") describes the rollout plan that was followed, not pending work. Open items are tracked separately on the companion `claude/add-reaction-support-jd0tU` branch (write path) and as a follow-up for custom-emoji admin CRUD.

*Replace the original embedded `MAP<TEXT, FROZEN<SET<FROZEN<"Participant">>>>` column on the four message tables with a flat, per-cell map keyed by `(emoji, user_account)`. The whole reactions map is read inline with the message row — no side table, no read-time fan-out.*

---

## 0. Pivot context & rollout plan

This is the **third design iteration** for reactions storage on the current branch (PR #221). The journey:
- **v1** (original main): embedded `MAP<TEXT, FROZEN<SET<FROZEN<"Participant">>>>` — rejected: every change rewrote the whole emoji's set.
- **v2** (the work currently on this PR): side table `message_reactions((message_id), emoji)`. Eliminates write amp but introduces N parallel reads per page → wrong direction for a read-heavy workload.
- **v3** (this spec): embedded `MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>` with a compound `(emoji, user_account)` map key. Each reaction is its own atomic map cell — no nested frozen collection, no whole-set rewrites, no read-time fan-out.

To preserve revertability, the implementation lands in **two commits within this PR**:
- **Commit A — Revert v2.** Strip all side-table code (`cassrepo/message_reactions*.go`, `service/reactions*.go`, the hydration wire sites, the regenerated mocks shrink, the env config, the `NewRepository` arity), restore `reactions` to `baseColumns` and thread columns (with v1 shape as a syntactic placeholder), delete the side-table DDL + the v2 migration.
- **Commit B — Introduce v3.** Add the two UDTs and the new column shape, switch the Go model field type, add the named-type marshaller, add the migration, update docs.

`git revert <B>` alone restores v2; `git revert <A> && git revert <B>` returns to current main.

For migration purposes this spec treats v2 as if it never landed on any database — the docker-local inline DDL goes straight v1 → v3, and a single migration script handles dev keyspaces still on v1.

## 1. Goal

Move reactions to a shape that satisfies all three of:

1. **Per-cell writes** — adding or removing a reaction is one atomic `UPDATE … SET reactions[k] = v` (or `DELETE reactions[k]`). No read-modify-write, no LWT-CAS.
2. **Inline reads** — reactions ride with the message row; no extra Cassandra round-trip per page.
3. **Self-uniqueness** — the map key encodes `(emoji, user_account)`, so DB-level uniqueness ("one user, one reaction per emoji") falls out of the schema.

### Pre-merge validation (mandatory)

Before locking the design, the implementer **must** ship a tiny `gocql` smoke test that round-trips `map[ReactionKey]ReactorInfo` through a real Cassandra container — read AND write. The codebase has zero precedent for a UDT struct as a map key, and `gocql`'s reflection path for that shape is historically a rough edge. If the smoke test fails or requires `MarshalUDT` / `UnmarshalUDT` methods on the structs, the spec adopts whichever approach works (mechanical: 10 lines per struct).

This validation lives under §8 "Tests" and is the first thing the implementation PR proves before touching production code.

## 2. Cassandra Schema

### 2.1 New UDTs

```cql
CREATE TYPE IF NOT EXISTS chat.reaction_key (
  emoji        TEXT,
  user_account TEXT
);

CREATE TYPE IF NOT EXISTS chat.reactor_info (
  user_id     TEXT,
  eng_name    TEXT,
  chn_name    TEXT,
  account     TEXT,
  reacted_at  TIMESTAMP
);
```

`reaction_key` is the map key — Cassandra requires map keys to be `FROZEN`. `reactor_info` rides as the value, also `FROZEN`, because the value is always set as a whole on add.

**Field naming** matches the `Participant` UDT precedent (`eng_name` / `chn_name`, not `english_name` / `chinese_name`).

**Caveat — frozen UDT extensibility.** Both UDTs are FROZEN. Adding a field to `reaction_key` later requires rewriting every map key on every row across the four message tables — effectively a full table backfill. Adding a field to `reactor_info` is easier (values can be lazily rewritten on next toggle) but still requires a migration. **Do not add or reorder fields on either UDT post-launch without a migration plan.**

`reaction_key.user_account` and `reactor_info.account` carry the same value. The duplication is intentional for read-side ergonomics (server-side scan yields a flat record without re-stitching the key). The wire shape (§6) collapses the duplication.

**Account immutability contract.** `user_account` is the load-bearing identity in `reaction_key`. If accounts can be renamed in this system, a stale `reaction_key` will be orphaned (subsequent un-react will silently fail to find the cell). Verified by inspecting `pkg/userstore`: accounts are immutable in this codebase. If that ever changes, this design needs to switch the key to `user_id`.

**Emoji normalisation contract.** Map-key equality is byte-exact. The same emoji can arrive in multiple valid Unicode encodings (NFC vs NFD, ZWJ sequences). The reaction handler **must** NFC-normalise `emoji` before binding into `reaction_key`. Document this in the addReaction PR; reading code can assume normalised values.

### 2.2 New column on the four message tables

For fresh keyspaces, the column appears inline in the `CREATE TABLE` statements (§4):

```cql
reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>,
```

For dev keyspaces already created, the migration (§4) uses:

```cql
ALTER TABLE chat.messages_by_room        ADD reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>;
ALTER TABLE chat.messages_by_id          ADD reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>;
ALTER TABLE chat.thread_messages_by_thread ADD reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>;
ALTER TABLE chat.pinned_messages_by_room ADD reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>;
```

The migration script uses `ADD IF NOT EXISTS` (Cassandra 4.0+) to stay idempotent — see §4.

### 2.3 Write semantics (out of scope for this PR — informative)

The map-key UDT is bound as a Go struct, not as a CQL literal — both safer and portable:

```cql
-- Add or replace one reaction
UPDATE messages_by_id
   SET reactions[?] = ?, updated_at = ?
 WHERE message_id = ? AND created_at = ?;

-- Remove one reaction
DELETE reactions[?]
  FROM messages_by_id
 WHERE message_id = ? AND created_at = ?;
```

Bind the first `?` to a `cassandra.ReactionKey` value (frozen UDT); bind the second to a `cassandra.ReactorInfo` value (frozen UDT). gocql encodes the frozen UDTs via reflection on `cql` tags. One map-cell write per reaction toggle.

### 2.4 Compaction & tombstones

- `messages_by_room` and `thread_messages_by_thread` — **TWCS** (time-windowed). Chat append patterns line up with TWCS's strength; the reactions column rides along.
- `messages_by_id` and `pinned_messages_by_room` — **LCS** (leveled). Both are point-lookup tables; LCS keeps read amplification bounded.

These compaction choices should be applied to the inline `CREATE TABLE` clauses in `docker-local/cassandra/init/10-13-*.cql`. The migration script in §4 cannot retro-apply them to existing dev keyspaces — devs who want the optimal compaction need to drop+recreate their local keyspace. Acceptable trade-off.

**Tombstone behaviour.** Each `DELETE reactions[k]` generates one map-cell tombstone, shadowing the live cell until `gc_grace_seconds` elapses (default 10 days). A hot message with churning reactors (add → remove → re-add cycles) accumulates tombstones inside the live row. Per-cell tombstones are the smallest possible shape (much better than v1's range tombstones), but the gc_grace window matters:

- **Reaction churn rate is low** in practice (a user rarely toggles the same reaction multiple times). Acceptable for v1.
- If we ever observe tombstone-driven read latency on hot rows, options include lowering `gc_grace_seconds` (risky with multi-DC repair windows) or running periodic `nodetool scrub`. Out of scope for this spec.

### 2.5 Mirror consistency

A single reaction toggle writes 2–4 mirror tables (`messages_by_id` always; `messages_by_room` always; `thread_messages_by_thread` when the message is a thread reply; `pinned_messages_by_room` when the message is pinned). The writes are **not atomic across tables** — there is no batch, no LWT.

**Source-of-truth contract:** `messages_by_id` is authoritative. Mirror tables are eventually-consistent. Readers MUST NOT diff reactions across mirrors; the addReaction handler is expected to write to `messages_by_id` first, then mirror to the others. Partial failure of a mirror write returns an error to the caller and gets retried at the application level (the addReaction PR's concern).

## 3. Go Models — `pkg/model/cassandra/`

### 3.1 New UDT carriers

In `pkg/model/cassandra/message.go`, above `Message`:

```go
// ReactionKey is the map-key UDT for Message.Reactions.
type ReactionKey struct {
    Emoji       string `json:"emoji"       cql:"emoji"`
    UserAccount string `json:"userAccount" cql:"user_account"`
}

// ReactorInfo is the map-value UDT for Message.Reactions.
type ReactorInfo struct {
    UserID    string    `json:"userId"    cql:"user_id"`
    EngName   string    `json:"engName"   cql:"eng_name"`
    ChnName   string    `json:"chnName"   cql:"chn_name"`
    Account   string    `json:"account"   cql:"account"`
    ReactedAt time.Time `json:"reactedAt" cql:"reacted_at"`
}
```

Naming matches `Participant`'s `EngName` convention exactly — no `EnglishName` / `ChineseName` drift. No `bson` tags (Cassandra-only carriers, matches package precedent).

> Tag alignment in this spec is illustrative; gofmt/goimports will reformat on commit.

### 3.2 Updated `Message.Reactions` field and named-type marshaller

The field uses a **named map type** so we can hang custom `MarshalJSON` / `UnmarshalJSON` on it (Go can't define methods on a built-in `map[K]V`):

```go
// Reactions is the in-row reaction map for Message. Keys are unique (emoji, user_account)
// pairs; one cell per reaction. JSON projection is grouped per emoji with a minimal per-user
// record — see MarshalJSON and §6 for the wire shape.
type Reactions map[ReactionKey]ReactorInfo

// MarshalJSON emits map<emoji, [{account, displayName}]>. Per-emoji arrays are sorted by
// reactedAt ASC (FIFO — oldest first), with account ASC breaking same-millisecond ties.
// Outer JSON object key order is unspecified. displayName is composed via
// displayfmt.CombineWithFallback(EngName, ChineseName, UserAccount).
// Nil → "null"; empty → "{}" (omitempty elides nil on Message).
func (r Reactions) MarshalJSON() ([]byte, error) { /* see §6 */ }
```

The wire is server→client one-way (clients are JS); there is no `UnmarshalJSON` and no `ErrDuplicateReactionKey` sentinel. The storage map already enforces `(emoji, account)` uniqueness via the Go type, so duplicate-key validation on the wire would be redundant.

The `Message.Reactions` field becomes:

```go
// Reactions is hydrated inline from the message row's reactions map column.
// Nil = no reactions (omitted from JSON); not modified by edit/delete paths.
Reactions Reactions `json:"reactions,omitempty" cql:"reactions"`
```

The `cql` tag is **restored**. `structScan` scans the column into the named map type — same reflection that handles `map[string]string`.

### 3.3 Removals from the v2 iteration

These v2 carriers are deleted in Commit A:

- `pkg/model/cassandra/message.go:71-76` — the `MessageReaction` struct.
- `pkg/model/cassandra/message_test.go:223-235` — `TestMessageReaction_JSONRoundTrip`.

Search regex (`grep -rn MessageReaction`) MUST return zero hits after Commit A.

### 3.4 Tests (in `pkg/model/cassandra/reactions_test.go`)

- `TestReactionKey_JSONRoundTrip` / `TestReactorInfo_JSONRoundTrip` — UDT carriers round-trip via default `json:` tags (no custom marshaller on the carriers themselves).
- `TestReactions_MarshalJSON/nil` — direct `json.Marshal(Reactions(nil))` → `"null"`.
- `TestReactions_MarshalJSON/nil_omitted_via_omitempty` — nil `Message.Reactions` produces no `"reactions"` key in the message JSON.
- `TestReactions_MarshalJSON/empty` — `Reactions{}` → `"{}"`.
- `TestReactions_MarshalJSON/single` — wire shape is `map<emoji, [{account, displayName}]>`.
- `TestReactions_MarshalJSON/grouped_by_emoji_fifo_by_reactedAt` — per-emoji arrays sorted by `reactedAt` ASC (FIFO, oldest first).
- `TestReactions_MarshalJSON/same_timestamp_breaks_by_account_asc` — same-millisecond ties break by `account` ASC.
- `TestReactions_MarshalJSON/displayName_fallback_to_account` — when both name fields are blank, `displayName` falls back to `account`.
- `TestReactions_MarshalJSON/one_user_multiple_different_emoji` and `/no_duplicate_account_within_emoji_bucket` — pin the spec §1 self-uniqueness contract at the wire level.

## 4. DDL Files — `docker-local/cassandra/init/`

**Commit A (revert v2):**
- Delete `docker-local/cassandra/init/14-table-message_reactions.cql`.
- Delete `docker-local/cassandra/init/90-migrate-drop-old-reactions-column.cql`.

**Commit B (introduce v3):**

Add:
- `07-udt-reaction_key.cql` — the new key UDT.
- `08-udt-reactor_info.cql` — the new value UDT.

Edit (restore the `reactions` column with the new shape; also set explicit compaction per §2.4):

- `10-table-messages_by_room.cql`
- `11-table-thread_messages_by_thread.cql`
- `12-table-pinned_messages_by_room.cql`
- `13-table-messages_by_id.cql`

Each gains a column line slotted exactly where the v1 column sat (after `visible_to`, before `deleted` — verify against `10-table-messages_by_room.cql:19-20` and siblings):

```cql
reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>,
```

And gains a `WITH compaction = {'class': '<TWCS|LCS>'}` clause per §2.4 (TWCS for the bucketed tables, LCS for `messages_by_id` and `pinned_messages_by_room`).

**Do NOT add an in-place `DROP reactions` + `ADD reactions` migration to `init/`.** A prior
revision shipped `90-migrate-reactions-to-v3.cql` as "fully idempotent"; it was neither, and
it aborted every `make deps-up` (init runs under `set -e`). The DROP+ADD pattern fails because
dropping a column records its type in `system_schema.dropped_columns` with UDTs expanded to
frozen tuples, so re-`ADD`ing the same name with the named-UDT type is rejected as
incompatible — regardless of whether the prior column was v1 or v3.

Apply the v3 shape per environment instead:

- **Fresh keyspace (dev/CI/new cluster):** none needed — the inline column in `10`–`13` is the
  source of truth.
- **Persistent dev keyspace on v1:** `docker compose -f docker-local/compose.deps.yaml down -v`
  then `make deps-up`. The shape change can't be done in place, and dev data is disposable.
- **Production:** not a DDL migration — needs an app-level read-translate-write backfill into a
  new column, owned by an ops runbook. Hand-applied DDL lives in
  `docker-local/cassandra/migrations/` (never globbed by `cassandra-init`).

## 5. Schema Docs — `docs/cassandra_message_model.md`

- Restore the `reactions` row on each of the four message-table sections; type column reads `MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>`.
- Drop the "Reaction table" section that the v2 iteration added.
- **Delete the v2-era exception note at line 70** ("Applies to … NOT to `message_reactions`") — becomes nonsense once the side table is gone.
- Add a "Reaction UDTs" subsection documenting `reaction_key` + `reactor_info` field by field, the immutability / extensibility caveats from §2.1, and the mirror-write source-of-truth contract from §2.5.
- Note the compaction strategy chosen per table per §2.4.

## 6. Client API — `docs/client-api.md`

**Wire shape: grouped by emoji, minimal per-user record.**

```json
"reactions": {
  "❤️": [{"account": "bob",   "displayName": "Bob 鲍勃"}],
  "👍": [{"account": "alice", "displayName": "Alice 爱丽丝"}, {"account": "carol", "displayName": "Carol 卡罗尔"}]
}
```

**Why grouped + minimal (architect feedback during PR #221 review):**
- Frontends render reactions as emoji buckets; grouping at the server saves the FE a per-message `groupBy` pass.
- Per-reactor payload shrinks to two fields, materially reducing NATS payload size on high-reaction messages.
- `displayName` is composed server-side via `displayfmt.CombineWithFallback(EngName, ChineseName, Account)` — the same helper already used by `room-worker/sysmsg.go`. Single source of truth for name formatting.
- `account` stays as the stable per-user identifier for FE concerns: "is this me?", "remove my reaction", React keys.
- The per-user `userId` / `eng_name` / `chn_name` / `reactedAt` fields stay in Cassandra storage (UDT shape unchanged — §2.1, §3.1) but are server-side details, never emitted on the wire.

**Ordering:** per-emoji arrays are sorted by `reactedAt` ascending (FIFO — oldest first, newest last), matching the legacy MongoDB array insertion-order behaviour. Same-millisecond ties break by `account` ASC for determinism. Outer JSON object key order (across emojis) is unspecified — the FE applies its own emoji ordering.

**Empty state:** nil `Reactions` → field omitted (via `omitempty`). Empty `Reactions{}` → `"reactions": {}`. The FE should treat both as "no reactions".

**Empirical FE impact today:** `grep -rni "reaction" chat-frontend/` returns zero hits. The frontend doesn't render reactions yet — the wire-shape change has no actual consumers to break. The doc update is forward-looking.

**Live-event shape vs history shape — known asymmetry.** The `add-reaction-support` branch emits `MessageReactedPayload{messageId, shortcode, action, actor, reactedAt}` for live updates — a single-actor delta carrying the toggling user's full `Participant` (display names included; the FE composes its own displayName for the delta if it wants to mirror the history shape). The history (this spec) returns the grouped minimal map above. Frontends merge a delta into their per-message state by adding or removing one entry under `reactions[shortcode]` keyed on `actor.account`.

Affected handlers: same six as before (`LoadHistory`, `LoadNextMessages`, `LoadSurroundingMessages`, `GetMessageByID`, `GetThreadMessages`, `GetThreadParentMessages`). Update `docs/client-api.md`'s description of the `reactions` field to the new shape; per CLAUDE.md §5 this update lands in the same PR.

## 7. `history-service` — Read Path (Commit A: v2 reversal)

The branch's side-table machinery is **removed** in Commit A:

- Delete `history-service/internal/cassrepo/message_reactions.go`, `message_reactions_integration_test.go`, `message_reactions_bench_test.go`, `repository_test.go` (the clamp test).
- Delete `history-service/internal/service/reactions.go`, `reactions_test.go`.
- Remove the 6 `hydrateReactions` / `GetReactionsByMessageID` call sites in `messages.go` + `threads.go`.
- Remove the 4 `_HydrateReactionsError` tests in `messages_test.go` and the 2 in `threads_test.go`.
- Remove the 5 `*_HydratesReactions` happy-path tests; reactions assertions move into the existing per-handler happy-path tests (since reactions ride with the row, the regular happy-path tests can seed + assert reactions in the same fixture).
- Revert `MessageReader` interface: drop `GetReactionsByMessageID` and `GetReactionsByMessageIDs`.
- Regenerate mocks (`make generate SERVICE=history-service`).
- Revert `NewRepository` to its 3-arg form `(session, bucket, maxBuckets)`. Drop the `reactionsConcurrency` field on `Repository` and the `< 1 → 1` clamp.
- Restore the `reactions` column to `baseColumns` in `messages_by_room.go` and to the column list in `thread_messages.go` (with v3 shape in Commit B; Commit A can leave it as a v1-shaped scan target since no real rows exist yet on the v3-shaped column — but cleanest is to do the whole-column restore in Commit B).
- Drop `REACTIONS_FETCH_CONCURRENCY` from `internal/config/config.go`.
- Update `cmd/main.go:93` to pass the 3-arg `NewRepository` shape; remove the `cfg.ReactionsFetchConcurrency` reference.

Update the ~30 test sites that pass `(session, bucket, 365, 50)` back to `(session, bucket, 365)`.

After Commit B a paged read returns reactions for free — no additional Cassandra round-trips, no errgroup, no concurrency cap.

## 8. Tests

### 8.0 gocql `map[UDT]UDT` smoke test (Commit B, FIRST)

Before any production-code changes in Commit B, add `pkg/model/cassandra/gocql_map_udt_smoke_test.go` (build-tag `integration`) that:

1. Creates a tiny `chat.reaction_smoke` test table with just `(message_id TEXT PRIMARY KEY, reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>)` and the two UDTs in an isolated keyspace via `testutil.CassandraKeyspace`.
2. Inserts a 2-entry reactions map via raw gocql `Query.Bind`.
3. `SELECT … reactions FROM chat.reaction_smoke` via raw `Iter.Scan(&out)` where `out` is `map[ReactionKey]ReactorInfo`.
4. Asserts equality round-trip on both entries.

If this test fails, the implementer **must** add `MarshalUDT` / `UnmarshalUDT` methods to `ReactionKey` and `ReactorInfo` (and add the same to this test's setup). The smoke test is the gate: design proceeds iff it passes.

### 8.1 Unit tests
Round-trip tests for `ReactionKey`, `ReactorInfo`, `Reactions` JSON marshalling — full enumeration in §3.4 (happy, empty, nil-via-omitempty, duplicate-key invalid input, malformed JSON).

The existing per-handler happy-path tests seed `Reactions` on their fixture messages and assert the field on the response. No `*_HydratesReactions` or `*_HydrateReactionsError` tests needed — reactions are in-row.

### 8.2 Integration tests
- An integration test that writes a row with a 2-entry reactions map (via raw CQL using bound UDT structs, per §2.3) and confirms `GetMessageByID` returns the deserialised map.
- Update the existing `*_integration_test.go` row-round-trip tests in `cassrepo/` to seed + assert on the new map shape (v2 stripped these blocks; we re-add them).

### 8.3 Concurrency / benchmark
- Delete `BenchmarkGetReactionsByMessageIDs` (no longer applicable).
- No fan-out concurrency to test.

Coverage targets unchanged: ≥80% floor / ≥90% on touched code per CLAUDE.md §4.

## 9. `setupCassandra` + `message-worker` — DDL parity

This is critical and was missed in the v2 spec. **Three** test files have inline schema that must mirror the production DDL:

1. **`history-service/internal/cassrepo/integration_test.go:15-141`** (the `setupCassandra` helper):
   - Delete the `message_reactions` `CREATE TABLE` block (lines 126-131).
   - Add `CREATE TYPE IF NOT EXISTS … reaction_key (…)` and `CREATE TYPE IF NOT EXISTS … reactor_info (…)` before the message tables.
   - Add the `reactions MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>` column to all four message-table `CREATE TABLE` strings.
   - Note: `pinned_messages_by_room` block in the helper is currently incomplete (missing most columns). Beyond scope to fix here, but flag.

2. **`history-service/internal/service/integration_test.go`** — same edits if it duplicates schema.

3. **`message-worker/integration_test.go`** — same. `message-worker`'s production INSERT statements do not currently set the reactions column (they never did; the column is populated by reaction writes, not message creation). Confirm and leave the INSERTs alone.

Search regex `reactions\s+MAP` catches all sites. After Commit B, no occurrences of the v1 or v2 shape should remain anywhere — the regex must hit only the four production DDL files + the three test files, all with the v3 shape.

## 10. Mocks

`make generate SERVICE=history-service` regenerates `internal/service/mocks/mock_repository.go` after `MessageReader` shrinks (Commit A) and the build settles (Commit B requires no further mock changes). Commit the regenerated file in Commit A.

## 11. Out of Scope — `addReaction` PR

The reaction-write path remains a separate PR. With the v3 shape it is **much simpler** than what the in-flight `add-reaction-support` branch currently has. The write path:

```cql
-- add
UPDATE messages_by_id
   SET reactions[?] = ?, updated_at = ?
 WHERE message_id = ? AND created_at = ?;

-- remove
DELETE reactions[?]
  FROM messages_by_id
 WHERE message_id = ? AND created_at = ?;
```

Plus the same `UPDATE` / `DELETE` against the 1–3 mirror tables the message lives in. One map-cell write per reaction op, per mirror. No LWT-CAS, no read-modify-write. Per §2.5, `messages_by_id` is the source of truth.

### Handoff to `claude/add-reaction-support-jd0tU`

Once this PR merges, the add-reaction branch's rebase delta:

- **Keep:** `pkg/emoji/*` (shortcode validator), `pkg/model/custom_emoji.go`, `pkg/model/event.go` reaction event additions (`EventReacted`, `MessageReactedPayload`), `pkg/subject/*` reaction subjects (`MsgReactPattern`, `MsgCanonicalReacted`), broadcast-worker `handleReacted` wiring, notification-worker reaction wiring, search-sync-worker reaction-skip.
- **Rewrite:** `history-service/internal/cassrepo/reactions.go` — entire LWT-based `ToggleReaction` collapses to the simple `UPDATE/DELETE` map-cell writes above. The four-table mirror logic remains; `messages_by_id` is written first.
- **Adjust:** `history-service/internal/service/reactions.go` — `ReactMessage` handler still resolves the actor, validates the shortcode (now via the gatekeeper's NFC-normalised emoji), but the read-before-write for `alreadyReacted` membership check becomes a simple map lookup on `msg.Reactions[ReactionKey{Emoji, UserAccount}]` since reactions are now in-row. The handler may need a small re-read of `msg` before the toggle if there's a race window, or can use `IF NOT EXISTS` / `IF EXISTS` on the map-cell write for toggle-correctness without read-modify-write.
- **Delete:** any code that depended on the v1 `Reactions map[string][]Participant` shape.

The canonical event pipeline (broadcast-worker → frontends, notification-worker, search-sync-worker skip) is untouched by this change.

### Edit / delete behaviour of the host message
- **Edit:** reactions are preserved (the field isn't touched by the edit handler).
- **Soft delete:** reactions stay on the row; rendered as "[deleted]" upstream, so they are effectively invisible without any code touching them.
- **Hard delete** (if/when introduced): reactions go away with the row. No cascade needed.

### Federation
Cross-site reaction propagation via OUTBOX/INBOX remains out of scope for both this PR and the addReaction PR. Documented as a known limitation.

## 12. Clean services (no changes needed in this PR)

`broadcast-worker`, `inbox-worker`, `notification-worker`, `room-service`, `room-worker`, `search-service`, `search-sync-worker`, `auth-service`, `mock-user-service`, `pkg/stream`, `pkg/subject` — no reaction references to touch.

## 13. Rollout

### Commit A — Revert v2

1. Delete side-table code (cassrepo + service files).
2. Remove hydration wire sites in `messages.go` + `threads.go`.
3. Remove v2-specific tests.
4. Shrink `MessageReader` interface.
5. Regenerate mocks.
6. Revert `NewRepository` arity.
7. Restore the `reactions` column to `baseColumns` / thread columns (placeholder type; Commit B brings the v3 shape).
8. Drop `REACTIONS_FETCH_CONCURRENCY` config.
9. Update `cmd/main.go` and the ~30 test call sites for the 3-arg constructor.
10. Delete `docker-local/cassandra/init/14-table-message_reactions.cql` and `90-migrate-drop-old-reactions-column.cql`.
11. Delete `pkg/model/cassandra/message.go:71-76` (`MessageReaction`) and `message_test.go:223-235`.

After Commit A: lint, build, and tests pass. Reactions are gone from the code entirely — the column exists in DDL as a placeholder (v1-shape syntactic match, no rows reading it because no readers exist).

### Commit B — Introduce v3

1. Add the gocql `map[UDT]UDT` smoke test (§8.0). **Verify it passes** before continuing.
2. Add the two UDTs (`07-udt-reaction_key.cql`, `08-udt-reactor_info.cql`).
3. Update the four message-table `CREATE TABLE` files: `reactions` column to v3 shape + explicit compaction strategy per §2.4.
4. (No in-place migration script — see §4. Fresh keyspaces get v3 from the inline column; persistent dev keyspaces reset via `down -v`.)
5. Add the Go types (`ReactionKey`, `ReactorInfo`, named `Reactions` type with `MarshalJSON` / `UnmarshalJSON`).
6. Change `Message.Reactions` field type to `Reactions`.
7. Add round-trip tests per §3.4.
8. Update integration test schemas per §9.
9. Update `docs/cassandra_message_model.md` per §5.
10. Update `docs/client-api.md` per §6 (flag breaking change).

### Acceptance criteria

- gocql `map[UDT]UDT` smoke test passes (the gate).
- All 6 history handlers return reactions inline on the message rows.
- `make fmt`, `make lint`, `make test`, `make test-integration SERVICE=history-service`, `make sast` all green.
- `make generate` produces no mock drift.
- `docs/cassandra_message_model.md` and `docs/client-api.md` updated; client-api flagged as breaking.
- `docs/reviews/` cleared before opening (per CLAUDE.md §5).
- Splitting into Commit A + Commit B is preserved; `git revert <B>` returns to v2; `git revert <A> <B>` returns to current main.

### Effort estimate

**2.5 days** for a focused implementer:
- Day 1: Commit A reversal + the gocql smoke test (gate validation).
- Day 1.5–2.5: Commit B v3 implementation + tests + docs + review-cycle churn.

The earlier "1 day" estimate didn't account for the ~30 call-site reversal, custom JSON marshaller, three inline DDL test files, mock regen, doc updates, and CI review churn.

## 14. Decisions Walked Back (for posterity)

This is the **third** iteration of the reactions storage design on this branch.

- **v1 — Original embedded `MAP<TEXT, FROZEN<SET<FROZEN<"Participant">>>>`.** Rejected mid-branch because every change to a single reaction required rewriting the whole emoji's set, generating range tombstones on the message row and accumulating read amplification.
- **v2 — Side table `message_reactions((message_id), emoji)`** (the initial direction of this branch). Eliminated write amplification by keeping reactions in one place, but introduced N parallel single-partition reads per page hydration. Workable but added complexity (`hydrateReactions` helper, errgroup, fan-out cap config) and shifted the cost to the read path — wrong direction for a read-heavy workload.
- **v3 — Embedded `MAP<FROZEN<reaction_key>, FROZEN<reactor_info>>`** (this spec). Keeps reactions inline (free read), uses a compound `(emoji, user_account)` map key so each reaction is its own atomic map cell (no nested frozen collection, no whole-set rewrites). Pays write amplification (mirror tables) but for a low-frequency event that's acceptable; pays zero read amplification. Self-uniqueness falls out of the map-key constraint. Denormalised reactor display info lives in the value so reads are render-ready.

The v2 iteration is being treated as if it never landed for migration purposes — the inline DDL goes straight from v1 to v3, and the migration script handles dev keyspaces still on v1.
