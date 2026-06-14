# Reactions Feature — Post-Merge Follow-up Changes

Tracking spec for follow-up changes raised in review of PR #258 (merged as
`e262e99b`, "feat(reactions): write path, validation, downstream fan-out").
Reviewer `mliu33` approved with three actionable suggestions. PR #273
(`notification-worker: gate pushes to regular messages + scope the canonical
consumer`, merged `e262e99b`) narrowed `notification-worker`'s consumer
filter to `{Created, Reacted}`; one of the items below carries that pattern
the next step.

This spec is approved before any implementation begins. Each item is
implemented as its own commit on a single branch (`claude/reactions-followups`)
which is squashed and rebased before push.

## Status at a glance

| Order | Item | Source | Status |
|---|---|---|---|
| 1 | Drop `updated_at` touch on `RemoveReaction` and batch the writes | mliu33 on `reactions.go:70` | Approved, implement |
| 2 | Move reaction author-notification to broadcast-worker; narrow notification-worker consumer filter to `{Created}` | mliu33 on `notification-worker/handler.go:315`; carries PR #273's principle | Approved, implement |
| 3 | Add `FindUserByAccount` singular; use it in the reaction handler | mliu33 on `reactions.go:53` | Approved, implement |

Items are independent of each other and can be implemented in any order; the
ordering above is just the commit sequence on the branch. No item changes
client wire format.

---

## Item 1 — Drop `updated_at` touch and batch the writes

### Why

**The touch is dead work.** No consumer in the reaction flow reads
`messages_by_id.updated_at` for reaction freshness. Broadcast events build
`ReactedAt`/`UpdatedAt` from the canonical event's `Message.UpdatedAt`
(which the handler sets from its local `reactedAt`, never read from
Cassandra). Search-sync-worker explicitly skips reaction events. Pagination
uses `created_at`. `LoadHistory` returns the full row including the
`reactions` map directly; no `If-Modified-Since` or ETag semantics exist in
NATS request/reply.

**Batching is precedented.** `cassrepo/pin.go:PinMessage` /
`UnpinMessage` already use `gocql.UnloggedBatch` to ship statements across
different partitions (`messages_by_id` + `pinned_messages_by_room`) in one
coordinator round-trip. The comment there documents the trade-off:
"transport grouping, not atomic; half-apply on coordinator failure is
possible (caller-side heal in service/pin.go)." Reactions are an even
stronger fit because the heal is automatically idempotent — a re-executed
DELETE on an already-deleted map cell is a no-op.

### RTT impact

| Operation | Current | After |
|---|---|---|
| `AddReaction` (top-level) | 2 RTTs | **1 RTT** |
| `AddReaction` (thread reply) | 2 RTTs | **1 RTT** |
| `RemoveReaction` (top-level) | 4 RTTs (DELETE + touch per table) | **1 RTT** |
| `RemoveReaction` (thread reply) | 4 RTTs | **1 RTT** |

Per RTT is ~1–5ms on intra-DC `LocalQuorum`. Real wall-clock savings:
~1–4ms on Add, ~3–15ms on Remove.

### Scope

#### `history-service/internal/cassrepo/reactions.go`

- Delete query constants `touchUpdatedAtMsgByID`, `touchUpdatedAtMsgByRoom`,
  `touchUpdatedAtThreadMsg`.
- Refactor `AddReaction` to issue one `gocql.UnloggedBatch` containing the
  `messages_by_id` statement plus the room-or-thread mirror statement.
- Refactor `RemoveReaction` to issue one `gocql.UnloggedBatch` containing
  the same two DELETEs.
- Match the comment style and error wrapping used in `cassrepo/pin.go`:
  `"… via batch(messages_by_id, messages_by_room): %w"`.
- Pre-flight check `ThreadParentID != "" && ThreadRoomID == ""` stays at
  the top of each method.

#### `history-service/internal/cassrepo/reactions_integration_test.go`

- Flip every assertion that expects `updated_at` to bump on a Remove. New
  expectation: the row's `updated_at` reflects the row's last
  edit/delete/create time, unchanged by the reaction.
- Add a regression test that AddReaction does still bump `updated_at` to
  `reactedAt` (Add's combined `UPDATE … SET reactions[?] = ?, updated_at = ?`
  statement still touches it).
- Existing pinned-message tests are unaffected: `pinned_messages_by_room`
  is not a reactions mirror.

#### `docs/specs/message-reactions.md`

- Update §2.5 ("Mirror consistency") to drop the four-statement Remove
  story; replace with the batched two-statement shape and the
  natural-idempotency heal pattern.
- Touch §2.5.1 ("Reactions-specific retry caveat") prose to clarify the
  retry-flip wrinkle stems from the handler's read-then-decide-direction
  step and is unaffected by batching.

#### `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md`

- Update §6 ("Store Interface — MessageWriter") to describe the new
  batched-write semantics and cite `cassrepo/pin.go` as the precedent.

### Tests

- `TestRepository_AddReaction_TopLevel` — assert reactions map cell is
  present in both tables; `updated_at` matches `reactedAt`.
- `TestRepository_AddReaction_ThreadReply` — same for thread-reply path.
- `TestRepository_RemoveReaction_TopLevel` — assert cell is gone from both
  tables; **assert `updated_at` is NOT bumped** by the remove.
- `TestRepository_RemoveReaction_ThreadReply` — same.
- `TestRepository_RemoveReaction_Idempotent_AbsentCell` — already covers
  DELETE-on-missing-cell; verify still passes.
- (Optional) `TestRepository_AddReaction_UnloggedBatch_GroupsBothStatements`
  if a clean introspection seam exists; skip if it would require gocql
  internals.

### Verification

- `make test SERVICE=history-service`
- `make test-integration SERVICE=history-service` (testcontainers, Cassandra)
- `make lint`
- `make sast`

### Acceptance criteria

- [ ] `AddReaction` issues one `gocql.UnloggedBatch` per call (2 statements).
- [ ] `RemoveReaction` issues one `gocql.UnloggedBatch` per call (2 statements).
- [ ] No `touchUpdatedAt*` query constants remain in `reactions.go`.
- [ ] Integration tests assert `updated_at` is NOT bumped on Remove.
- [ ] `make test`, `make lint`, `make sast` clean.
- [ ] Spec docs updated.

### References

- Review comment: <https://github.com/hmchangw/chat/pull/258#discussion_r3366774917>
- Precedent: `history-service/internal/cassrepo/pin.go:34-65`

---

## Item 2 — Move reaction notification to broadcast-worker, narrow consumer filter

### Why

After PR #237 (`notification-worker: cache + mobile push overhaul`) and
PR #273 (`notification-worker: gate pushes to regular messages + scope the
canonical consumer`), `notification-worker` is conceptually a pure mobile
push pipeline. Its `HandlerDeps` carries `MemberCache`, `Presence`, `Hook`,
batched `Emitter`. Our reaction notification bypasses all of that via a
bespoke `Publisher` field and a direct NATS publish to
`chat.user.{authorAccount}.notification`. It is the only thing in the worker
that does not fit the push pipeline.

PR #273 narrowed the durable consumer's `FilterSubjects` to
`{MsgCanonicalCreated, MsgCanonicalReacted}` so the worker stops being
delivered events it never acts on. Moving the reaction notification out
makes the next narrowing — to `{MsgCanonicalCreated}` only — the natural
follow-through.

`broadcast-worker.handleReacted` already consumes `EventReacted` to publish
`ReactRoomEvent` to the room. Adding the author-only notification publish
on the same code path consolidates all reaction wire effects into one worker.

**FE compatibility:** the wire subject and payload for the author
notification stay identical (`chat.user.{authorAccount}.notification`
carrying `NotificationEvent{Type: "reaction", …}`). FE distinguishes this
subject from `chat.room.{id}.event` in its UX (badge counter / notification
center routing) and we will not unify them.

### Scope

#### notification-worker

- **`notification-worker/bootstrap.go`** — `buildConsumerConfig`: remove
  `subject.MsgCanonicalReacted(siteID)` from `cc.FilterSubjects`. Result:
  `cc.FilterSubjects = []string{subject.MsgCanonicalCreated(siteID)}`.
- **`notification-worker/handler.go`**:
  - Remove `case model.EventReacted:` from `HandleMessage`'s switch.
  - Delete `handleReaction`.
  - Delete the `Publisher` interface.
  - Delete `ReactionPub Publisher` from `HandlerDeps`.
  - Delete `natsPublisher` wrapper type.
  - Drop the `subject` import; drop the `nats.Conn` import if no longer
    used after removing `natsPublisher`.
- **`notification-worker/main.go`**: drop
  `ReactionPub: natsPublisher{nc: nc.NatsConn()}` from `NewHandler(HandlerDeps{…})`.
- **`notification-worker/handler_test.go`**:
  - Delete the four `TestHandleMessage_Reaction_*` tests.
  - Delete `mockReactionPub`, `reactionPubRecord`, `reactHandler` helpers.
  - Update `TestBuildConsumerConfig`: expected `FilterSubjects` slice
    becomes `{MsgCanonicalCreated}` (length 1).

#### broadcast-worker

- **`broadcast-worker/handler.go`** — extend `handleReacted`:
  - After the existing `publishMutation` call (room fan-out), check
    policy: `evt.ReactionDelta.Action == model.ReactionActionAdded` AND
    `evt.Message.UserAccount != ""` AND
    `evt.Message.UserAccount != evt.ReactionDelta.Actor.Account`.
  - When policy holds, build a `model.NotificationEvent{Type: "reaction",
    RoomID, Message, ReactionDelta, Timestamp}` and publish on
    `subject.Notification(evt.Message.UserAccount)`.
  - **Failure semantics:** publish failure is logged at `slog.Error` with
    `request_id`, `messageID`, `roomID`, `siteID`. The handler returns
    nil to JetStream — the room event has already gone out and is the
    primary effect; NAK-ing would re-broadcast the room fan-out
    unnecessarily.
- Confirm broadcast-worker has a NATS publisher available; reuse if so.
  Otherwise add a thin helper backed by `*nats.Conn`, same shape as
  notification-worker's deleted `natsPublisher`.

#### broadcast-worker tests

- Port the four reaction-policy tests over with adjusted setup.
- Add a regression test: notification-publish failure does not prevent
  the room event from being fanned out, and `HandleMessage` returns nil.

#### Docs

- **`docs/specs/message-reactions.md` §10** ("Downstream consumers"): drop
  the `notification-worker` row; extend the `broadcast-worker` row with
  the author-notification responsibility.
- **`docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md`
  §3.2 / §3.3**: redraw the producer/consumer table.
- **`docs/superpowers/specs/2026-06-04-notification-worker-system-message-allowlist-design.md`**:
  add a follow-up note that `MsgCanonicalReacted` is removed from the
  filter as part of this change.

### Deploy ordering

Not applicable — the app has not shipped to production yet. The constraint
that would otherwise apply (broadcast-worker must roll out before
notification-worker to avoid a notification outage during the transition
window) is a no-op in a greenfield deploy: both services come up together
in their final state. Re-evaluate if these changes are later applied as an
incremental upgrade against an existing production deployment.

### Verification

- `make test SERVICE=notification-worker`
- `make test SERVICE=broadcast-worker`
- `make test-integration`
- `make lint`
- `make sast`
- Local NATS check: confirm notification-worker durable's filter updates
  in place.

### Acceptance criteria

- [ ] `notification-worker` no longer contains any `EventReacted` branch,
      `handleReaction` function, `Publisher` interface, `ReactionPub`
      field, or `natsPublisher` wrapper.
- [ ] `notification-worker`'s `FilterSubjects` is exactly
      `[]string{subject.MsgCanonicalCreated(siteID)}`.
- [ ] `broadcast-worker.handleReacted` publishes the author notification
      on `subject.Notification(authorAccount)` when policy holds.
- [ ] Author-notification policy is identical to the prior
      notification-worker behavior: action == added, author non-empty,
      author != actor.
- [ ] Notification publish failure is logged and swallowed; does not
      NAK the canonical event.
- [ ] FE receives reaction notifications on the same subject with the
      same payload (zero wire change).
- [ ] Four reaction tests pass in `broadcast-worker`.
- [ ] `TestBuildConsumerConfig` on `notification-worker` reflects the
      narrowed filter.
- [ ] Docs updated.
- [ ] `make test`, `make lint`, `make sast` clean.

### References

- Review comment: <https://github.com/hmchangw/chat/pull/258#discussion_r3367439840>
- `FilterSubjects` narrowing precedent: PR #273
- Notification-worker overhaul: PR #237

---

## Item 3 — `FindUserByAccount` singular for the reaction handler

### Why

The reaction handler currently calls:

```go
users, err := s.users.FindUsersByAccounts(c, []string{account})
if len(users) == 0 { ... }
actor := users[0]
```

A `[]string{}` wrap plus a `[0]` unwrap to say "fetch one user by account."
Misleading at the call site (reads as a batch when it's not).

Mongo's query planner rewrites a single-element `$in` to an equality
match — same `IXSCAN`, same key bounds, same 1 doc examined. So the
perf delta is essentially zero; this change is justified on API clarity
and error-semantic improvement (the singular form can return
`ErrUserNotFound` directly instead of forcing a separate `len() == 0` check).

Audit of `FindUsersByAccounts` call sites confirms the reaction handler
is the only 1-element caller; `broadcast-worker` and `room-service` are
genuine batches.

### Scope

#### `pkg/userstore/userstore.go`

- Add to `UserStore` interface:
  ```go
  FindUserByAccount(ctx context.Context, account string) (*model.User, error)
  ```
- Implement on `mongoStore`: `col.FindOne(ctx, bson.M{"account": account}, …)`
  with the same projection as `FindUsersByAccounts`. Returns
  `ErrUserNotFound` (wrapped) on no match, matching `FindUserByID`'s
  error semantics.

#### `pkg/userstore/cache.go`

- Add `FindUserByAccount(ctx, account)` on `Cache`:
  - Serve from the existing `byAccount` LRU when hot.
  - On miss, singleflight with an account-prefixed key
    (`"account:"+account`) so it doesn't collide with `FindUserByID`'s
    id-keyed group.
  - On store success, call existing `populate(u)` so both `byID` and
    `byAccount` LRUs hold the same pointer — a later `FindUserByID` for
    the same user hits the cache.
  - `ErrUserNotFound` propagates unwrapped; missing entries are NOT
    negatively cached (matches `FindUserByID`'s behavior).

#### `pkg/userstore/cache_test.go`

- Add `fakeStore.FindUserByAccount` impl (mirrors `FindUserByID`).
- Add four tests:
  - `TestCache_FindUserByAccount_MissThenHit`
  - `TestCache_FindUserByAccount_NotFoundIsUnwrapped`
  - `TestCache_FindUserByAccount_StoreErrorWrapped`
  - `TestCache_FindUserByAccount_CrossPopulatesByID`

#### `history-service/internal/service/service.go`

- Replace `FindUsersByAccounts` in the service-level `UserStore` interface
  with `FindUserByAccount`. The service-level interface is consumer-
  defined and should reflect only the methods actually used.

#### `history-service/internal/service/reactions.go`

- Replace the call site:
  ```go
  actor, err := s.users.FindUserByAccount(c, account)
  if err != nil {
      if errors.Is(err, userstore.ErrUserNotFound) {
          slog.WarnContext(c, "react: actor not found", "account", account)
          return nil, fmt.Errorf("react: actor not found for account %s", account)
      }
      return nil, fmt.Errorf("react: resolve actor %s: %w", account, err)
  }
  ```
- `actor` is now `*pkgmodel.User`; field accesses (`actor.Account`,
  `actor.EngName`, etc.) work via Go's auto-deref.
- Add the `userstore` import for `ErrUserNotFound`.

#### `history-service/internal/service/reactions_test.go`

- Rewrite every `FindUsersByAccounts(..., []string{"u1"}).Return(...)`
  mock expectation to `FindUserByAccount(..., "u1").Return(...)` with the
  right return shape (pointer or `ErrUserNotFound`).
- Add an `aliceUserPtr()` helper that returns `*model.User`. Keep
  `aliceUser()` (value) for tests that build the Cassandra `ReactorInfo`.
- Add the `userstore` import.

#### Mock regen

`make generate` regenerates `MockUserStore` in:
- `broadcast-worker/mock_userstore_test.go`
- `message-worker/mock_userstore_test.go`
- `history-service/internal/service/mocks/mock_repository.go`

These services don't call `FindUserByAccount` from production code, but
the mock acquires the new method automatically. No service-test changes
elsewhere.

### Audit done

The reaction handler is the ONLY 1-element `FindUsersByAccounts` call
site in production code:
- `broadcast-worker/handler.go`: dedup'd sender + mentions, genuine batch.
- `room-service/handler.go`: outbox fan-out across N subscriptions,
  genuine batch.

`FindUsersByAccounts` stays on `UserStore` and the relevant service
interfaces; this item only adds the singular alongside it.

### Reply to reviewer's perf framing

Before this lands, reply on the review thread:

> Agreed on the API shape — `[]string{account}` for a one-element batch
> reads as "this is batched" when it isn't, so the dedicated singular
> method is the right shape regardless. Quick question on the performance
> premise, because I want to make sure I'm sizing the win correctly
> before adding the parallel method: MongoDB's query planner rewrites a
> single-element `$in` to an equality match before execution — same
> `IXSCAN` plan, same index, same key bounds, same `totalKeysExamined`
> and `totalDocsExamined` per `explain("executionStats")`. The only
> measurable cost difference is a few microseconds of `$in` planning +
> BSON-encoding the 1-element array on the client side, well below
> network RTT noise. Are you seeing a workload where this shows up as
> measurable, or is the suggestion primarily about API shape? If the
> latter, totally agreed and I'll add `FindUserByAccount` and flip the
> reaction handler.

Ship on API-clarity grounds regardless of his reply.

### Verification

- `go build ./...`
- `make fmt`
- `make lint`
- `go test -race ./...`
- `go vet -tags=integration ./...`
- `make sast`

### Acceptance criteria

- [ ] `FindUserByAccount` exists on `pkg/userstore.UserStore` with the
      expected signature.
- [ ] `mongoStore.FindUserByAccount` uses `FindOne` with the projection
      matching `FindUsersByAccounts`; returns `ErrUserNotFound` on miss.
- [ ] `Cache.FindUserByAccount` consults `byAccount` LRU first;
      singleflight on miss; cross-populates `byID`.
- [ ] Four `TestCache_FindUserByAccount_*` tests pass.
- [ ] Reaction handler uses `FindUserByAccount` with `ErrUserNotFound`
      branching.
- [ ] No `FindUsersByAccounts([]string{singleAccount})` call sites
      remain in the repo.
- [ ] All mocks regenerated; `go build ./...` clean.
- [ ] `make test`, `make lint`, `make sast` clean.

### References

- Review comment: <https://github.com/hmchangw/chat/pull/258#discussion_r3366804706>

---

## Branch and commit plan

- Branch: `claude/reactions-followups` off current `origin/main`.
- Commits, in this order:
  1. `docs: spec for reactions post-merge follow-up changes` — this file.
  2. `cassrepo/reactions: drop updated_at touch and batch the writes` —
     Item 1.
  3. `notification-worker, broadcast-worker: move reaction notification
     to broadcast-worker, narrow consumer filter` — Item 2.
  4. `userstore: add FindUserByAccount singular, use it in reaction
     handler` — Item 3.
- Before push: rebase onto current `origin/main`, then squash items 2–4
  into a single commit if the reviewer prefers a single follow-up commit;
  otherwise keep separate commits for per-item revert granularity.

## Out of scope

CodeRabbit bot comments on PR #258 that we are deliberately not addressing
in this follow-up:

- CQL `//` comment syntax in `12-table-pinned_messages_by_room.cql` and
  `docs/cassandra_message_model.md` — pre-existing in the codebase; the
  init parser tolerates it; tracked as a doc-hygiene task separately.
- Add `slog.Error` for nil-delta fallback in `CanonicalDedupID` — both
  consumers already log at the handler boundary; adding a third log site
  at the dedup helper would be redundant.
- Move service interfaces from `service.go` to `store.go` and unexport
  them — the existing layout was inherited from before #258; refactoring
  it is a cross-service consistency pass for a separate PR.
- Use `getRecords()` instead of `pub.records` in one notification-worker
  test — gets fixed incidentally in Item 2's test relocation.
- NFC test in `pkg/emoji/emoji_test.go` where both `precomposed` and
  `decomposed` literals are byte-identical in source — valid bug; bundle
  the one-line fix into Item 3's commit (touched in the same PR).

## References

- PR #258 (merged): <https://github.com/hmchangw/chat/pull/258>
- Review comments by mliu33:
  - <https://github.com/hmchangw/chat/pull/258#discussion_r3366774917>
  - <https://github.com/hmchangw/chat/pull/258#discussion_r3367439840>
  - <https://github.com/hmchangw/chat/pull/258#discussion_r3366804706>
- PR #273 (`notification-worker: gate pushes to regular messages + scope
  the canonical consumer`): pattern for `FilterSubjects` narrowing.
- PR #237 (`notification-worker: cache + mobile push overhaul`): the
  rewrite that made the worker mobile-push-focused.
- Original reactions feature spec: `docs/specs/message-reactions.md`.
- Original reactions design doc:
  `docs/superpowers/specs/2026-05-28-message-reactions-write-path-design.md`.
