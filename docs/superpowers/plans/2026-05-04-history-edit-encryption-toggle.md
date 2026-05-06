# History-Service Edit-Message Encryption Toggle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `ENCRYPTION_ENABLED` env var (default `false`) to `history-service` that gates the Valkey connection and the encrypted-edit publish path. Make `encryptEditMsg` fail-closed: any encryption failure logs an error and skips the live `MessageEditedEvent` instead of falling back to plaintext.

**Architecture:** Mirrors the broadcast-worker toggle. New `EncryptionConfig` sub-struct with `envPrefix:"ENCRYPTION_"`. `cmd/main.go` only constructs `keyStore` when `Enabled=true`. `HistoryService` gains an `encrypt bool` field; `encryptEditMsg`'s signature gains an `error` return so the `EditMessage` caller can distinguish "ok to publish" from "encryption required but failed". When the toggle is off, `keyProvider` stays nil and the function early-returns plaintext.

**Tech Stack:** Go 1.25, `caarlos0/env/v11`, `go.uber.org/mock`, `stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-05-04-history-edit-encryption-toggle-design.md`

**Branch:** `claude/history-edit-encryption-toggle`

---

## File Structure

Modify only:
- `history-service/internal/config/config.go` — `EncryptionConfig` struct + `Encryption` field; relax `,required` on `Valkey.Addr`.
- `history-service/cmd/main.go` — conditional Valkey wiring + conditional shutdown hook + startup-log encryption flag + pass flag to `service.New`.
- `history-service/internal/service/service.go` — `encrypt` field on `HistoryService` + updated `New` constructor signature.
- `history-service/internal/service/messages.go` — new `encryptEditMsg` signature/body + updated `EditMessage` caller block.
- `history-service/internal/service/messages_test.go` — `newService` helper signature; sed-update 74 call sites; rewrite three existing tests that asserted today's plaintext-fallback behavior; add new tests for the toggle-off and skip-publish paths.
- `history-service/deploy/docker-compose.yml` — explicit `ENCRYPTION_ENABLED=false`.

No new files. No mock regeneration (the mocked interfaces — `RoomKeyProvider` etc. — are unchanged).

---

## Existing Tests Affected by the Behavioral Change

The sed replacement is mechanical for **71 of 74** call sites. The other **3 tests assert today's plaintext-fallback semantics and must be rewritten** — they currently exercise the very behavior we're changing:

1. **`TestHistoryService_EditMessage_Success` (around line 799)** — uses `keys.EXPECT().Get(...).Return(nil, nil)` to force the plaintext fallback, then expects `Publish` to be called with `evt.NewMsg=="new content"`. After the change, with `encrypt=true`, the nil-key branch returns an error and the caller skips publish. **Resolution:** retire this test — the existing `TestHistoryService_EditMessage_Success_EncryptedEvent` (real key + encrypted publish) is already the canonical happy-path test for encrypt=true.

2. **`TestHistoryService_EditMessage_PublishFails` (around line 1020)** — same nil-key + plaintext pattern but asserts the handler tolerates publisher errors. **Resolution:** keep the test's intent (publisher-error handling) but rewrite to provide a real key so encryption succeeds and the publish is attempted; assertion changes from "evt.NewMsg=='new content'" to "evt.EncryptedNewMsg is set".

3. **`TestHistoryService_EditMessage_Success_KeyFetchError` (around line 1087)** — explicitly tests "key error → plaintext fallback + publish". **Resolution:** invert the test — same key-fetch-error setup, but assert `pub.Publish` is NEVER called. Rename to `TestHistoryService_EditMessage_SkipPublishOnKeyError`.

These three rewrites land in the same commit as the `encryptEditMsg` change (Task 2) so the branch never has a red state.

---

## Task 1: Add `encrypt` flag plumbing (no behavior change)

**Files:**
- Modify: `history-service/internal/service/service.go` (struct around lines 65-86, constructor `New`)
- Modify: `history-service/internal/service/messages_test.go` (helper at line 52, sed all `newService(t)` call sites)
- Modify: `history-service/cmd/main.go` (constructor call at line 84 only — config wiring lands in Task 3)

This commit changes ONLY the service constructor signature and the helper, then sed-updates every existing test call site to pass `true`. `encryptEditMsg` is unchanged here, so all existing tests still pass — including the three "plaintext-fallback" tests, which we update in Task 2.

- [ ] **Step 1.1: Update `HistoryService` struct and `New`**

In `history-service/internal/service/service.go`, find the struct (currently around lines 65-75 — verify with `grep -n "type HistoryService struct\|func New(" history-service/internal/service/service.go`). Add `encrypt bool` as the last field, and add `encrypt bool` as the last parameter of `New`:

```go
type HistoryService struct {
	msgs          MessageRepository
	subs          SubscriptionRepository
	publisher     EventPublisher
	threadRooms   ThreadRoomRepository
	keyProvider   RoomKeyProvider
	encrypt       bool
}

func New(msgs MessageRepository, subs SubscriptionRepository, pub EventPublisher,
	threadRooms ThreadRoomRepository, keyProvider RoomKeyProvider, encrypt bool) *HistoryService {
	return &HistoryService{
		msgs:        msgs,
		subs:        subs,
		publisher:   pub,
		threadRooms: threadRooms,
		keyProvider: keyProvider,
		encrypt:     encrypt,
	}
}
```

(Match the existing struct's exact field ordering; add `encrypt` after `keyProvider`.)

- [ ] **Step 1.2: Update the test helper signature**

In `history-service/internal/service/messages_test.go`, find `newService` (currently at line 52):

```go
func newService(t *testing.T) (*service.HistoryService, *mocks.MockMessageRepository, *mocks.MockSubscriptionRepository, *mocks.MockEventPublisher, *mocks.MockThreadRoomRepository, *mocks.MockRoomKeyProvider) {
	// ...
	return service.New(msgs, subs, pub, threadRooms, keys), msgs, subs, pub, threadRooms, keys
}
```

Replace with:

```go
func newService(t *testing.T, encrypt bool) (*service.HistoryService, *mocks.MockMessageRepository, *mocks.MockSubscriptionRepository, *mocks.MockEventPublisher, *mocks.MockThreadRoomRepository, *mocks.MockRoomKeyProvider) {
	// ...
	return service.New(msgs, subs, pub, threadRooms, keys, encrypt), msgs, subs, pub, threadRooms, keys
}
```

(Body unchanged except for the `service.New(...)` call.)

- [ ] **Step 1.3: Sed-update all 74 call sites**

```bash
cd /home/user/chat
grep -c "newService(t)" history-service/internal/service/messages_test.go   # expect 74
sed -i 's/newService(t)/newService(t, true)/g' history-service/internal/service/messages_test.go
grep -c "newService(t)" history-service/internal/service/messages_test.go   # expect 0
grep -c "newService(t, true)" history-service/internal/service/messages_test.go  # expect 74
```

- [ ] **Step 1.4: Update the production call site in `cmd/main.go`**

The current line 84 is:

```go
	svc := service.New(cassRepo, subRepo, pub, threadRoomRepo, keyStore)
```

Replace with:

```go
	svc := service.New(cassRepo, subRepo, pub, threadRoomRepo, keyStore, true)
```

(Hardcoded `true` for now; Task 3 replaces with `cfg.Encryption.Enabled`. This pattern matches what we did in the broadcast-worker rollout — keeps each commit independently building.)

- [ ] **Step 1.5: Run unit tests + lint**

```bash
cd /home/user/chat
make test SERVICE=history-service
make lint
```

Expected: PASS (`encrypt=true` is the default everywhere, so `encryptEditMsg`'s existing behavior is unchanged → all 74 tests still pass).

- [ ] **Step 1.6: Commit**

```bash
git add history-service/internal/service/service.go history-service/internal/service/messages_test.go history-service/cmd/main.go
git commit -m "$(cat <<'EOF'
feat(history-service): add encrypt flag to HistoryService

Plumb a new `encrypt bool` field through HistoryService and its constructor.
All existing tests pass `true` to preserve current behavior. encryptEditMsg
itself is unchanged in this commit — the fail-closed semantics land next.

Spec: docs/superpowers/specs/2026-05-04-history-edit-encryption-toggle-design.md
EOF
)"
```

---

## Task 2: Make `encryptEditMsg` fail-closed (TDD)

**Files:**
- Modify: `history-service/internal/service/messages.go` (`encryptEditMsg` lines 209-236; `EditMessage` caller block lines 282-301)
- Modify: `history-service/internal/service/messages_test.go` (rewrite 3 existing tests; add new tests at end of edit-message section)

This is the security fix. We change `encryptEditMsg`'s contract to return an error, update the caller to skip publish on encryption error, and rewrite the three existing tests that asserted the old plaintext-fallback semantics.

- [ ] **Step 2.1: Update `encryptEditMsg` signature and body**

In `history-service/internal/service/messages.go`, replace the entire `encryptEditMsg` function (currently lines 209-236, including the comment block above it) with:

```go
// encryptEditMsg returns the payload pieces for the live MessageEditedEvent.
//
// Returns:
//   (plaintext, nil, nil)   — encryption is disabled, caller publishes plaintext.
//   ("",        encJSON, nil) — encryption succeeded, caller publishes encrypted.
//   ("",        nil,     err) — encryption was required but failed; caller MUST
//                                skip publishing to avoid plaintext exposure.
func (s *HistoryService) encryptEditMsg(c *natsrouter.Context, roomID, plaintext string) (string, json.RawMessage, error) {
	if !s.encrypt {
		return plaintext, nil, nil
	}
	key, err := s.keyProvider.Get(c, roomID)
	if err != nil {
		return "", nil, fmt.Errorf("get room key for room %s: %w", roomID, err)
	}
	if key == nil {
		return "", nil, fmt.Errorf("no current key for room %s", roomID)
	}
	encrypted, err := roomcrypto.Encode(plaintext, key.KeyPair.PublicKey, key.Version)
	if err != nil {
		return "", nil, fmt.Errorf("encrypt edit message for room %s: %w", roomID, err)
	}
	encJSON, err := json.Marshal(encrypted)
	if err != nil {
		return "", nil, fmt.Errorf("marshal encrypted edit message: %w", err)
	}
	return "", json.RawMessage(encJSON), nil
}
```

`fmt` is NOT currently imported in `messages.go` — add it. The current import block is:

```go
import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/hmchangw/chat/history-service/internal/cassrepo"
	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/roomcrypto"
	"github.com/hmchangw/chat/pkg/subject"
)
```

Add `"fmt"` to the standard-library group:

```go
import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hmchangw/chat/history-service/internal/cassrepo"
	"github.com/hmchangw/chat/history-service/internal/models"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/roomcrypto"
	"github.com/hmchangw/chat/pkg/subject"
)
```

- [ ] **Step 2.2: Update the `EditMessage` caller block**

In `history-service/internal/service/messages.go`, find the existing block (currently lines 282-301):

```go
	// Publish live event (best-effort — publish failure is logged, not returned).
	editedAtMs := editedAt.UnixMilli()
	plainMsg, encMsg := s.encryptEditMsg(c, roomID, req.NewMsg)
	evt := models.MessageEditedEvent{
		Type:            "message_edited",
		Timestamp:       editedAtMs,
		RoomID:          roomID,
		MessageID:       req.MessageID,
		NewMsg:          plainMsg,
		EncryptedNewMsg: encMsg,
		EditedBy:        account,
		EditedAt:        editedAtMs,
	}
	if payload, err := json.Marshal(evt); err == nil {
		if pubErr := s.publisher.Publish(c, subject.RoomEvent(roomID), payload); pubErr != nil {
			slog.Warn("edit: publish event failed", "error", pubErr, "messageID", req.MessageID)
		}
	} else {
		slog.Warn("edit: marshal event failed", "error", err, "messageID", req.MessageID)
	}
```

Replace with:

```go
	// Publish live event (best-effort — publish failure is logged, not returned).
	// When encryption is required but fails, skip the publish entirely to avoid
	// leaking plaintext on the live event subject. The Cassandra write already
	// succeeded; clients will see the edit on next history fetch.
	editedAtMs := editedAt.UnixMilli()
	plainMsg, encMsg, encErr := s.encryptEditMsg(c, roomID, req.NewMsg)
	if encErr != nil {
		slog.Error("edit: encryption failed, skipping live event to avoid plaintext exposure",
			"error", encErr, "messageID", req.MessageID, "roomID", roomID)
	} else {
		evt := models.MessageEditedEvent{
			Type:            "message_edited",
			Timestamp:       editedAtMs,
			RoomID:          roomID,
			MessageID:       req.MessageID,
			NewMsg:          plainMsg,
			EncryptedNewMsg: encMsg,
			EditedBy:        account,
			EditedAt:        editedAtMs,
		}
		if payload, err := json.Marshal(evt); err == nil {
			if pubErr := s.publisher.Publish(c, subject.RoomEvent(roomID), payload); pubErr != nil {
				slog.Warn("edit: publish event failed", "error", pubErr, "messageID", req.MessageID)
			}
		} else {
			slog.Warn("edit: marshal event failed", "error", err, "messageID", req.MessageID)
		}
	}
```

- [ ] **Step 2.3: Build to confirm compilation**

```bash
cd /home/user/chat
go build ./history-service/...
```

Expected: build succeeds (the messages.go internal change is consistent — only this file references `encryptEditMsg`).

- [ ] **Step 2.4: Run tests — three should now FAIL**

```bash
cd /home/user/chat
make test SERVICE=history-service
```

Expected failures:
- `TestHistoryService_EditMessage_Success` — calls `Publish` mock but the new code doesn't call it (nil key path now skips publish).
- `TestHistoryService_EditMessage_PublishFails` — same reason.
- `TestHistoryService_EditMessage_Success_KeyFetchError` — same reason.

These confirm the new behavior. Now we update the tests.

- [ ] **Step 2.5: Retire `TestHistoryService_EditMessage_Success`**

In `history-service/internal/service/messages_test.go`, **delete the entire `TestHistoryService_EditMessage_Success` function** (currently lines 799-845). The existing `TestHistoryService_EditMessage_Success_EncryptedEvent` (around line 1049) already covers the encrypt=true happy path with a real key — keeping both would be duplicate coverage with confused names.

Remove the function and the comment line `// --- EditMessage ---` should remain immediately before whatever test now starts that section (likely `_NotSubscribed`).

- [ ] **Step 2.6: Update `TestHistoryService_EditMessage_PublishFails`**

Find the test (currently around lines 1020-1044). Replace its body with one that uses a real key so encryption succeeds and the publisher-error path is actually reached:

```go
func TestHistoryService_EditMessage_PublishFails(t *testing.T) {
	svc, msgs, subs, pub, _, keys := newService(t, true)
	c := testContext()

	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)

	hydrated := &models.Message{
		MessageID: "m-abc",
		RoomID:    "r1",
		Sender:    models.Participant{Account: "u1"},
	}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-abc").Return(hydrated, nil)
	msgs.EXPECT().UpdateMessageContent(gomock.Any(), hydrated, "new content", gomock.Any()).Return(nil)

	// Real key so encryption succeeds and the publish is attempted.
	kp := generateTestKeyPair(t)
	keys.EXPECT().Get(gomock.Any(), "r1").Return(kp, nil)

	// Publisher fails, but handler must still return success (best-effort fan-out).
	pub.EXPECT().Publish(gomock.Any(), "chat.room.r1.event", gomock.Any()).Return(fmt.Errorf("nats disconnected"))

	resp, err := svc.EditMessage(c, models.EditMessageRequest{MessageID: "m-abc", NewMsg: "new content"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "m-abc", resp.MessageID)
}
```

- [ ] **Step 2.7: Invert `TestHistoryService_EditMessage_Success_KeyFetchError`**

Find the test (currently around lines 1087-1115). Rename it to `TestHistoryService_EditMessage_SkipPublishOnKeyError` and replace its body with one that asserts `Publish` is NEVER called:

```go
// TestHistoryService_EditMessage_SkipPublishOnKeyError verifies that when
// encryption is enabled and the keystore returns an error, the handler skips
// publishing the live event entirely (rather than leaking plaintext via the
// MessageEditedEvent.NewMsg field). The Cassandra write already succeeded.
func TestHistoryService_EditMessage_SkipPublishOnKeyError(t *testing.T) {
	svc, msgs, subs, pub, _, keys := newService(t, true)
	c := testContext()

	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	hydrated := &models.Message{
		MessageID: "m-abc",
		RoomID:    "r1",
		Sender:    models.Participant{Account: "u1"},
	}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-abc").Return(hydrated, nil)
	msgs.EXPECT().UpdateMessageContent(gomock.Any(), hydrated, "some edit", gomock.Any()).Return(nil)

	keys.EXPECT().Get(gomock.Any(), "r1").Return(nil, fmt.Errorf("valkey connection refused"))

	// pub.EXPECT().Publish is intentionally NOT set — gomock's strict mode will
	// fail the test if Publish is called.
	_ = pub

	resp, err := svc.EditMessage(c, models.EditMessageRequest{MessageID: "m-abc", NewMsg: "some edit"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "m-abc", resp.MessageID)
}
```

- [ ] **Step 2.8: Add new tests for the toggle-off and skip-publish-on-nil-key paths**

Append these tests to `history-service/internal/service/messages_test.go` (immediately after `TestHistoryService_EditMessage_SkipPublishOnKeyError`, before the `// --- DeleteMessage ---` divider):

```go
// TestHistoryService_EditMessage_PlaintextWhenDisabled verifies that with
// encryption disabled the handler publishes a plaintext MessageEditedEvent
// (NewMsg set, EncryptedNewMsg empty) and does not consult the key provider.
func TestHistoryService_EditMessage_PlaintextWhenDisabled(t *testing.T) {
	svc, msgs, subs, pub, _, _ := newService(t, false)
	c := testContext()

	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	hydrated := &models.Message{
		MessageID: "m-abc",
		RoomID:    "r1",
		Sender:    models.Participant{Account: "u1"},
	}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-abc").Return(hydrated, nil)
	msgs.EXPECT().UpdateMessageContent(gomock.Any(), hydrated, "plain edit", gomock.Any()).Return(nil)

	// No keys.EXPECT().Get — the disabled path must not consult the provider.

	pub.EXPECT().
		Publish(gomock.Any(), "chat.room.r1.event", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, data []byte) error {
			var evt models.MessageEditedEvent
			require.NoError(t, json.Unmarshal(data, &evt))
			assert.Equal(t, "plain edit", evt.NewMsg)
			assert.Empty(t, evt.EncryptedNewMsg)
			return nil
		})

	resp, err := svc.EditMessage(c, models.EditMessageRequest{MessageID: "m-abc", NewMsg: "plain edit"})
	require.NoError(t, err)
	assert.Equal(t, "m-abc", resp.MessageID)
}

// TestHistoryService_EditMessage_SkipPublishOnNilKey verifies that when
// encryption is enabled and the keystore returns (nil, nil), the handler
// skips publishing the live event rather than falling back to plaintext.
func TestHistoryService_EditMessage_SkipPublishOnNilKey(t *testing.T) {
	svc, msgs, subs, pub, _, keys := newService(t, true)
	c := testContext()

	subs.EXPECT().GetHistorySharedSince(gomock.Any(), "u1", "r1").Return(nil, true, nil)
	hydrated := &models.Message{
		MessageID: "m-abc",
		RoomID:    "r1",
		Sender:    models.Participant{Account: "u1"},
	}
	msgs.EXPECT().GetMessageByID(gomock.Any(), "m-abc").Return(hydrated, nil)
	msgs.EXPECT().UpdateMessageContent(gomock.Any(), hydrated, "secret", gomock.Any()).Return(nil)

	keys.EXPECT().Get(gomock.Any(), "r1").Return(nil, nil)

	// pub.EXPECT().Publish is intentionally NOT set.
	_ = pub

	resp, err := svc.EditMessage(c, models.EditMessageRequest{MessageID: "m-abc", NewMsg: "secret"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "m-abc", resp.MessageID)
}
```

- [ ] **Step 2.9: Add direct unit tests for `encryptEditMsg`**

Append to `history-service/internal/service/messages_test.go` (also before `// --- DeleteMessage ---`):

```go
// TestHistoryService_encryptEditMsg covers the four return shapes of
// encryptEditMsg directly (in addition to the EditMessage end-to-end tests).
func TestHistoryService_encryptEditMsg(t *testing.T) {
	t.Run("disabled returns plaintext", func(t *testing.T) {
		svc, _, _, _, _, keys := newService(t, false)
		// No keys.EXPECT().Get — must not be consulted.
		_ = keys
		c := testContext()
		plain, enc, err := svc.EncryptEditMsgForTest(c, "r1", "hello")
		require.NoError(t, err)
		assert.Equal(t, "hello", plain)
		assert.Nil(t, enc)
	})

	t.Run("enabled with valid key returns encrypted JSON", func(t *testing.T) {
		svc, _, _, _, _, keys := newService(t, true)
		kp := generateTestKeyPair(t)
		keys.EXPECT().Get(gomock.Any(), "r1").Return(kp, nil)
		c := testContext()
		plain, enc, err := svc.EncryptEditMsgForTest(c, "r1", "secret")
		require.NoError(t, err)
		assert.Empty(t, plain)
		assert.NotEmpty(t, enc)
		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(enc, &decoded))
	})

	t.Run("enabled with key fetch error returns error", func(t *testing.T) {
		svc, _, _, _, _, keys := newService(t, true)
		keys.EXPECT().Get(gomock.Any(), "r1").Return(nil, fmt.Errorf("valkey down"))
		c := testContext()
		plain, enc, err := svc.EncryptEditMsgForTest(c, "r1", "secret")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "valkey down")
		assert.Empty(t, plain)
		assert.Nil(t, enc)
	})

	t.Run("enabled with nil key returns error", func(t *testing.T) {
		svc, _, _, _, _, keys := newService(t, true)
		keys.EXPECT().Get(gomock.Any(), "r1").Return(nil, nil)
		c := testContext()
		plain, enc, err := svc.EncryptEditMsgForTest(c, "r1", "secret")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no current key")
		assert.Empty(t, plain)
		assert.Nil(t, enc)
	})
}
```

`encryptEditMsg` is unexported and `messages_test.go` lives in the **external** `package service_test`, so the tests cannot call it directly. Add a test-only export shim in a new file `history-service/internal/service/export_test.go`:

```go
package service

import (
	"encoding/json"

	"github.com/hmchangw/chat/pkg/natsrouter"
)

// EncryptEditMsgForTest is exported only for tests in another package.
// The `_test.go` suffix means this file is included only in test builds,
// so the shim never ships in production binaries.
func (s *HistoryService) EncryptEditMsgForTest(c *natsrouter.Context, roomID, plaintext string) (string, json.RawMessage, error) {
	return s.encryptEditMsg(c, roomID, plaintext)
}
```

Because the file ends in `_test.go`, Go's build excludes it from production binaries — this satisfies CLAUDE.md's "test helpers belong in `_test.go` files only" rule.

- [ ] **Step 2.10: Run tests + lint**

```bash
cd /home/user/chat
make test SERVICE=history-service
make lint
```

Expected: PASS, including the four new subtests of `TestHistoryService_encryptEditMsg`, the two new EditMessage tests, and the two rewritten EditMessage tests.

- [ ] **Step 2.11: Commit**

```bash
git add history-service/internal/service/messages.go \
        history-service/internal/service/messages_test.go \
        history-service/internal/service/export_test.go
git commit -m "$(cat <<'EOF'
feat(history-service): fail-closed encryption for edit-message events

Change encryptEditMsg to return an error on any encryption failure so the
EditMessage caller can skip publishing the live event entirely instead of
falling back to plaintext. Closes a plaintext-leak path that fired whenever
Valkey was unavailable, the room had no key, or the encode step failed.

The Cassandra write still succeeds before the publish attempt, so clients
see the edit on their next history fetch. Updates three existing tests that
asserted the old plaintext-fallback behavior; adds direct coverage of
encryptEditMsg's four return shapes and end-to-end coverage of the
disabled/skip-publish paths.

Spec: docs/superpowers/specs/2026-05-04-history-edit-encryption-toggle-design.md
EOF
)"
```

---

## Task 3: Wire `ENCRYPTION_ENABLED` (config + main.go + docker-compose)

**Files:**
- Modify: `history-service/internal/config/config.go`
- Modify: `history-service/cmd/main.go`
- Modify: `history-service/deploy/docker-compose.yml`

- [ ] **Step 3.1: Add `EncryptionConfig` and `Encryption` field; relax `Valkey.Addr`**

In `history-service/internal/config/config.go`:

Find the existing `ValkeyConfig`:

```go
// ValkeyConfig holds Valkey (Redis-compatible) connection settings.
// Env vars: VALKEY_ADDR, VALKEY_PASSWORD
type ValkeyConfig struct {
	Addr     string `env:"ADDR" required:"true"`
	Password string `env:"PASSWORD" envDefault:""`
}
```

Replace with:

```go
// ValkeyConfig holds Valkey (Redis-compatible) connection settings.
// Env vars: VALKEY_ADDR, VALKEY_PASSWORD
// Addr is validated only when encryption is enabled (see main.go).
type ValkeyConfig struct {
	Addr     string `env:"ADDR"`
	Password string `env:"PASSWORD" envDefault:""`
}

// EncryptionConfig gates the room-key (Valkey) connection and the
// encrypted-edit publish path.
// Env vars: ENCRYPTION_ENABLED
type EncryptionConfig struct {
	Enabled bool `env:"ENABLED" envDefault:"false"`
}
```

Find the `Config` struct:

```go
type Config struct {
	SiteID    string          `env:"SITE_ID" envDefault:"site-local"`
	Cassandra CassandraConfig `envPrefix:"CASSANDRA_"`
	Mongo     MongoConfig     `envPrefix:"MONGO_"`
	NATS      NATSConfig      `envPrefix:"NATS_"`
	Valkey    ValkeyConfig    `envPrefix:"VALKEY_"`
}
```

Replace with:

```go
type Config struct {
	SiteID     string           `env:"SITE_ID" envDefault:"site-local"`
	Cassandra  CassandraConfig  `envPrefix:"CASSANDRA_"`
	Mongo      MongoConfig      `envPrefix:"MONGO_"`
	NATS       NATSConfig       `envPrefix:"NATS_"`
	Valkey     ValkeyConfig     `envPrefix:"VALKEY_"`
	Encryption EncryptionConfig `envPrefix:"ENCRYPTION_"`
}
```

- [ ] **Step 3.2: Replace Valkey wiring with conditional block in `cmd/main.go`**

In `history-service/cmd/main.go`, find the existing block (currently lines 63-71):

```go
	keyStore, err := roomkeystore.NewValkeyStore(roomkeystore.Config{
		Addr:        cfg.Valkey.Addr,
		Password:    cfg.Valkey.Password,
		GracePeriod: 0, // history-service never rotates keys; grace period is irrelevant
	})
	if err != nil {
		slog.Error("valkey connect failed", "error", err)
		os.Exit(1)
	}
```

Replace with:

```go
	var keyStore roomkeystore.RoomKeyStore
	if cfg.Encryption.Enabled {
		if cfg.Valkey.Addr == "" {
			slog.Error("encryption enabled but VALKEY_ADDR is empty")
			os.Exit(1)
		}
		keyStore, err = roomkeystore.NewValkeyStore(roomkeystore.Config{
			Addr:        cfg.Valkey.Addr,
			Password:    cfg.Valkey.Password,
			GracePeriod: 0, // history-service never rotates keys; grace period is irrelevant
		})
		if err != nil {
			slog.Error("valkey connect failed", "error", err)
			os.Exit(1)
		}
	}
```

- [ ] **Step 3.3: Pass the encryption flag to `service.New`**

Find the line that currently reads:

```go
	svc := service.New(cassRepo, subRepo, pub, threadRoomRepo, keyStore, true)
```

(After Task 1 it has the hardcoded `true`.) Replace with:

```go
	svc := service.New(cassRepo, subRepo, pub, threadRoomRepo, keyStore, cfg.Encryption.Enabled)
```

- [ ] **Step 3.4: Add `encryption` field to startup log**

Find:

```go
	slog.Info("history-service running", "site", cfg.SiteID)
```

Replace with:

```go
	slog.Info("history-service running", "site", cfg.SiteID, "encryption", cfg.Encryption.Enabled)
```

- [ ] **Step 3.5: Make the `keyStore.Close()` shutdown hook conditional**

Find the `shutdown.Wait` call (currently lines 93-100). It passes hooks as variadic args including `func(ctx context.Context) error { return keyStore.Close() }` unconditionally. `pkg/shutdown.Wait` has signature `func Wait(ctx context.Context, timeout time.Duration, shutdownFuncs ...func(context.Context) error)` — no exported `Hook` alias. Refactor to a slice:

```go
	hooks := []func(context.Context) error{
		func(ctx context.Context) error { return router.Shutdown(ctx) },
		func(ctx context.Context) error { return nc.Drain() },
		func(ctx context.Context) error { return tracerShutdown(ctx) },
	}
	if keyStore != nil {
		hooks = append(hooks, func(ctx context.Context) error { return keyStore.Close() })
	}
	hooks = append(hooks,
		func(ctx context.Context) error { mongoutil.Disconnect(ctx, mongoClient); return nil },
		func(ctx context.Context) error { cassutil.Close(cassSession); return nil },
	)
	shutdown.Wait(ctx, 25*time.Second, hooks...)
```

(Preserves the existing hook order: router → nats → tracer → keyStore (conditional) → mongo → cassandra.)

- [ ] **Step 3.6: Build to confirm compilation**

```bash
cd /home/user/chat
go build ./history-service/...
```

Expected: succeeds.

- [ ] **Step 3.7: Update `history-service/deploy/docker-compose.yml`**

Find the broadcast service's `environment:` block (lines 15-22):

```yaml
    environment:
      - NATS_URL=nats://nats:4222
      - NATS_CREDS_FILE=/etc/nats/backend.creds
      - SITE_ID=site-local
      - MONGO_URI=mongodb://mongodb:27017
      - MONGO_DB=chat
      - CASSANDRA_HOSTS=cassandra
      - CASSANDRA_KEYSPACE=chat
      - VALKEY_ADDR=valkey:6379
```

Append `ENCRYPTION_ENABLED=false` immediately after the `VALKEY_ADDR` line:

```yaml
      - VALKEY_ADDR=valkey:6379
      - ENCRYPTION_ENABLED=false
```

(Keep the `valkey` service definition and the `VALKEY_ADDR` env var so devs who flip the toggle can exercise the encrypted path locally.)

- [ ] **Step 3.8: Run unit tests + lint**

```bash
cd /home/user/chat
make test SERVICE=history-service
make lint
```

Expected: PASS, no findings.

- [ ] **Step 3.9: Commit**

```bash
git add history-service/internal/config/config.go history-service/cmd/main.go history-service/deploy/docker-compose.yml
git commit -m "$(cat <<'EOF'
feat(history-service): gate encryption + Valkey behind ENCRYPTION_ENABLED

Add EncryptionConfig (envPrefix ENCRYPTION_) with a single Enabled bool,
default false. When disabled, history-service no longer connects to Valkey
and skips its close hook on shutdown; encryptEditMsg short-circuits to
plaintext. When enabled, behavior is unchanged from previous commits and
VALKEY_ADDR is validated at startup. docker-compose.yml sets
ENCRYPTION_ENABLED=false so dev defaults match prod.

Spec: docs/superpowers/specs/2026-05-04-history-edit-encryption-toggle-design.md
EOF
)"
```

---

## Task 4: Push the branch

- [ ] **Step 4.1: Push**

```bash
git push -u origin claude/history-edit-encryption-toggle
```

Expected: branch updated on remote with the spec commit + Task 1 + Task 2 + Task 3 (4 commits total).

---

## Self-Review (against the spec)

- **Spec coverage:**
  - `EncryptionConfig` struct + `Encryption` field → Task 3 Step 3.1.
  - `Valkey.Addr` `,required` removal → Task 3 Step 3.1.
  - Conditional Valkey wiring + nil-safe shutdown hook → Task 3 Steps 3.2, 3.5.
  - `service.New` signature change → Task 1 Step 1.1.
  - Helper sed update → Task 1 Steps 1.2, 1.3.
  - Production call-site update → Task 1 Step 1.4 (initial `true`), Task 3 Step 3.3 (final `cfg.Encryption.Enabled`).
  - `encryptEditMsg` fail-closed semantics → Task 2 Steps 2.1, 2.2.
  - Three rewritten existing tests → Task 2 Steps 2.5, 2.6, 2.7.
  - New EditMessage end-to-end tests (plaintext, skip-on-key-error, skip-on-nil-key) → Task 2 Steps 2.7, 2.8.
  - Direct `encryptEditMsg` unit tests → Task 2 Step 2.9.
  - docker-compose default → Task 3 Step 3.7.
  - Startup log encryption flag → Task 3 Step 3.4.
- **Placeholder scan:** none — every step has concrete code/commands and expected output.
- **Type consistency:** `HistoryService.encrypt` (unexported), `New(..., encrypt bool)`, env tag `ENABLED` under prefix `ENCRYPTION_` → env var `ENCRYPTION_ENABLED` — consistent across spec, plan, and broadcast-worker precedent.
