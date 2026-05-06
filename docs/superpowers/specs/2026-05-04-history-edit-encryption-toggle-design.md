# History-Service Edit-Message Encryption Toggle — Design

**Date:** 2026-05-04
**Service:** `history-service`
**Branch:** `claude/history-edit-encryption-toggle`
**Related work:** mirrors `broadcast-worker` toggle (spec `2026-05-03-broadcast-encryption-toggle-design.md`).

## Summary

Add an `ENCRYPTION_ENABLED` env var (default `false`) to `history-service` that gates both the room-key (Valkey) connection and the encrypted-edit live-event publish path. When the toggle is on, `encryptEditMsg` becomes fail-closed: any encryption failure logs an error and skips the live event entirely instead of falling back to plaintext, eliminating a plaintext-leak path under "encryption required" mode.

## Motivation

`encryptEditMsg` (history-service/internal/service/messages.go:213) currently fails open: on Valkey error, missing key, encrypt error, or marshal error it logs WARN and publishes the live `MessageEditedEvent` with the plaintext `NewMsg` field set. That path quietly defeats encryption. Operators need:

1. The ability to run `history-service` without Valkey at all (matches the broadcast-worker default-off posture for fresh sites and local dev).
2. A guarantee that, when encryption is required, plaintext does not get published to the live-event subject if encryption fails.

## Configuration

A new sub-struct in `history-service/internal/config/config.go`:

```go
// EncryptionConfig gates the room-key (Valkey) connection and the encrypted-edit publish path.
// Env vars: ENCRYPTION_ENABLED
type EncryptionConfig struct {
    Enabled bool `env:"ENABLED" envDefault:"false"`
}
```

Added to the top-level `Config` with `envPrefix:"ENCRYPTION_"`:

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

`ValkeyConfig.Addr` loses its `required:"true"` tag so the service can start without Valkey when encryption is off:

```go
type ValkeyConfig struct {
    Addr     string `env:"ADDR"`
    Password string `env:"PASSWORD" envDefault:""`
}
```

When the toggle is on but `Valkey.Addr` is empty, validation in `cmd/main.go` logs an error and exits non-zero. (Matches the broadcast-worker pattern.)

## Wiring (`cmd/main.go`)

Combined validation + Valkey connect + nil-shutdown-hook block, single flag check:

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
        GracePeriod: 0, // history-service never rotates keys
    })
    if err != nil {
        slog.Error("valkey connect failed", "error", err)
        os.Exit(1)
    }
}
```

`service.New(...)` gains a final `encrypt bool` argument (`cfg.Encryption.Enabled`).

The shutdown hooks are refactored to a `[]func(context.Context) error` slice so `keyStore.Close()` is appended only when `keyStore != nil`. Order is preserved: router shutdown → nats drain → tracer → keyStore.Close (conditional) → mongo disconnect → cassandra close.

The startup log gains an `encryption` field.

## Service & `encryptEditMsg` Semantics

`HistoryService` (in `internal/service/service.go`) gains an `encrypt bool` field. Constructor:

```go
func New(msgs MessageRepository, subs SubscriptionRepository, pub EventPublisher,
         threadRooms ThreadRoomRepository, keyProvider RoomKeyProvider,
         encrypt bool) *HistoryService
```

`encryptEditMsg` (currently returning `(plaintext, json.RawMessage)`) gains an error return so the caller can distinguish "ok to publish" from "encryption required but failed":

```go
// Returns:
//   (plaintext, nil, nil)   when encryption is disabled — caller publishes plaintext.
//   ("",        encJSON, nil) when encryption succeeded — caller publishes encrypted.
//   ("",        nil,     err) when encryption was required but failed — caller MUST skip publish.
func (s *HistoryService) encryptEditMsg(c *natsrouter.Context, roomID, plaintext string) (string, json.RawMessage, error)
```

Body:

```go
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
```

When `s.encrypt=false`, `s.keyProvider` is `nil` (set that way at startup) but the function never reaches the dereference — the early `return` on the `!s.encrypt` branch guards it.

## Caller Change (`EditMessage`)

The relevant block (currently messages.go:283–301) becomes:

```go
plainMsg, encMsg, encErr := s.encryptEditMsg(c, roomID, req.NewMsg)
if encErr != nil {
    slog.Error("edit: encryption failed, skipping live event to avoid plaintext exposure",
        "error", encErr, "messageID", req.MessageID, "roomID", roomID)
    // Cassandra write already persisted; clients see the edit on next history fetch.
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

Cassandra `UpdateMessageContent` still runs *before* this block — the message is durably edited regardless of publish outcome.

## Local Development

`history-service/deploy/docker-compose.yml`: add explicit `ENCRYPTION_ENABLED=false` to the service `environment:` block. Devs who want to exercise the encrypted path opt in by overriding the variable; the existing Valkey service in compose stays defined.

## Tests

### Existing unit tests — mechanical update

The 74 existing `newService(t)` call sites in `internal/service/messages_test.go` become `newService(t, true)`. The helper signature changes:

```go
func newService(t *testing.T, encrypt bool) (*service.HistoryService, ..., *mocks.MockRoomKeyProvider) {
    // ...
    return service.New(msgs, subs, pub, threadRooms, keys, encrypt), msgs, subs, pub, threadRooms, keys
}
```

A single `sed` replacement covers all call sites.

### New unit tests for `encryptEditMsg` (in `messages_test.go`)

| Test | Setup | Asserts |
|------|-------|---------|
| `TestHistoryService_encryptEditMsg_Disabled` | `newService(t, false)` | returns `(plaintext, nil, nil)`; `keyProvider.Get` is never called |
| `TestHistoryService_encryptEditMsg_Success` | `newService(t, true)`, mock returns valid key | returns `("", encJSON, nil)`; `encJSON` decodes as a valid `roomcrypto.EncryptedMessage` with the right Version |
| `TestHistoryService_encryptEditMsg_KeyError` | `newService(t, true)`, mock returns `(nil, errors.New("valkey down"))` | returns `("", nil, err)`; `err.Error()` contains "valkey down" |
| `TestHistoryService_encryptEditMsg_NilKey` | `newService(t, true)`, mock returns `(nil, nil)` | returns `("", nil, err)`; `err.Error()` mentions "no current key" |

### New `EditMessage` end-to-end tests

| Test | Setup | Asserts |
|------|-------|---------|
| `TestHistoryService_EditMessage_PlaintextWhenDisabled` | `newService(t, false)` | one event published; `NewMsg == "new content"`, `EncryptedNewMsg == nil` |
| `TestHistoryService_EditMessage_SkipPublishOnEncryptError` | `newService(t, true)`, key mock returns Valkey error | `UpdateMessageContent` is called; `publisher.Publish` is NEVER called |
| `TestHistoryService_EditMessage_SkipPublishOnNilKey` | `newService(t, true)`, key mock returns nil key | same as above — no publish |

The skip-publish tests prove the security fix: with the toggle on, an encryption failure produces zero bytes on the room-event subject.

### Coverage

Per CLAUDE.md, target ≥80% across the package and ≥90% on service code. The new branch is small and the new tests cover it directly. The existing `EditMessage` tests for unrelated paths (NotSubscribed, NotFound, AlreadyDeleted, etc.) stay intact via the sed update.

## Consumer Impact

`MessageEditedEvent` already carries both `NewMsg` (string) and `EncryptedNewMsg` (json.RawMessage). With the toggle off, edit events shift to `NewMsg=plaintext`, `EncryptedNewMsg=nil` — the same shape produced before encryption was introduced. With the toggle on, behavior is unchanged from today **except** that encryption failures now produce no event (instead of a plaintext fallback). Any subscriber that relied on the plaintext-fallback path will see live edit events disappear when encryption is misconfigured — that is the intended security trade-off; clients still see the edit on their next history fetch since Cassandra has already been updated.

The chat frontend lives in a separate repo. Coordinating consumers is out of scope.

## Out of Scope

- `DeleteMessage` path (the user explicitly scoped this to edit events).
- Integration tests (history-service's tests are unit-only against mocks).
- Renaming or restructuring the existing `VALKEY_*` env vars.
- Frontend changes.
- Metrics for skipped-publish counts.

## Files Touched

- `history-service/internal/config/config.go` — `EncryptionConfig`, `Encryption` field on `Config`, drop `required:"true"` on `Valkey.Addr`.
- `history-service/cmd/main.go` — conditional Valkey wiring + conditional shutdown hook + startup log + pass `cfg.Encryption.Enabled` to `service.New`.
- `history-service/internal/service/service.go` — `encrypt` field, updated `New` constructor signature.
- `history-service/internal/service/messages.go` — new `encryptEditMsg` signature and body; updated `EditMessage` caller block.
- `history-service/internal/service/messages_test.go` — `newService` signature update + sed for all call sites; new tests for `encryptEditMsg` and `EditMessage` toggle behavior.
- `history-service/deploy/docker-compose.yml` — explicit `ENCRYPTION_ENABLED=false`.
