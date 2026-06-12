# Large-Room Bot Scaling Loadgen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `max-room-size` loadgen subcommand that ramps room size while a bot sends at a fixed RPS, and reports the largest room size that holds SLO (gating on notification-worker backlog as the headline O(N) signal).

**Architecture:** A new `botroom` workload in `tools/loadgen` (single `package main`). Seeds large channel rooms (up to ~5000 members) directly into Mongo + per-room keys in Valkey, with a bot seeded as room owner so it bypasses the gatekeeper large-room post restriction. A new subcommand ramps over a `--sizes` list: per size step it warms up, holds while a paced bot sender publishes `--rate` msgs/sec round-robin across `--rooms-per-size` rooms, polls JetStream consumer pending at hold start/end, and produces a PASS/TRIP/INCONCLUSIVE verdict. Reuses the existing `Collector` (publish→broadcast correlation via `RoomEvent.LastMsgID`), `pacedDispatch` pacer, `pollPending`/`diffPending`, `snapshotSelfMetrics`, `ComputePercentiles`, and seed plumbing.

**Tech Stack:** Go 1.25, NATS + JetStream (`nats.go`, `nats.go/jetstream`), MongoDB (`mongo-driver/v2`), Valkey (`pkg/roomkeystore`), Prometheus client, `stretchr/testify`, `go.uber.org/mock`, testcontainers via `pkg/testutil`.

---

## File Structure

**New files (all `package main` in `tools/loadgen/`):**
- `seed_botroom.go` — `BotRoomPreset`, `BuildBotRoomFixtures`, `BotRoomLayout`, preset registry.
- `seed_botroom_test.go` — fixture determinism + shape tests.
- `botroom.go` — `BotRoomSender` (paced bot sender) and the optional read driver.
- `botroom_test.go` — sender unit tests with a stub publisher.
- `botroom_verdict.go` — `BotRoomThresholds`, `BotRoomStepResult`, `evaluateBotRoomStep`, gated-durable list.
- `botroom_verdict_test.go` — verdict table tests.
- `botroom_report.go` — per-step table, ANSWER line, CSV writer.
- `botroom_report_test.go` — render + CSV tests.
- `botroom_integration_test.go` — `//go:build integration` end-to-end.

**Modified files:**
- `main.go` — `max-room-size` subcommand; `botroom` case in `seed`/`teardown` `--workload`.
- `metrics.go` — `loadgen_botroom_*` metric families.
- `members.go` — `members-capacity-xl` preset (companion add-path bump).
- `deploy/docker-compose.yml` — `MAX_ROOM_SIZE` 1000 → 6000 on room-service.
- `deploy/Makefile` — `seed-botroom`, `run-max-room-size`, `teardown-botroom`.
- `README.md` — large-room bot scenario section.

**Reused as-is (no edits — verified signatures):**
- `collector.go`: `NewCollector(m *Metrics, preset string) *Collector`, `RecordPublish(requestID, messageID string, t time.Time)`, `RecordBroadcast(messageID string, at time.Time)`, `DiscardBefore(cutoff time.Time)`, `E2Samples() []time.Duration`, `E2Count() int`.
- `main.go`: `newE2Handler(collector *Collector) func(*nats.Msg)`, `connectStores(ctx, cfg) (*mongo.Database, roomkeystore.RoomKeyStore, func(), error)`, `gatheredCounterValue(...)`.
- `pacer.go`: `pacedDispatch(ctx, rate int, maxInFlight int, onUnderrun func(int), onSaturated func(), publishOne func(context.Context))`.
- `daily_verdict.go`: `pollPending(ctx, jszURL string) (map[string]int64, error)`, `diffPending(start, end map[string]int64) map[string]ConsumerPendingDelta`, `ConsumerPendingDelta{Start, End, Delta int64}`, `snapshotSelfMetrics() SelfMetrics`, `SelfMetrics{GCPauseP99Ms, CPUPercent float64; Goroutines int}`.
- `daily.go`: `parseStepList(s string) ([]int, error)`.
- `report.go`: `ComputePercentiles(samples []time.Duration) Percentiles`, `Percentiles{P50, P95, P99, Max time.Duration}`, `ConsumerStat`.
- `consumerlag.go`: `NewConsumerSampler(js, stream, durable string, m *Metrics, interval time.Duration) *ConsumerSampler`.
- `seed.go`: `Seed(ctx, db, *Fixtures) error`, `Teardown(ctx, db) error`, `SeedRoomKeys(ctx, keys roomKeyStore, roomKeys map[string]roomkeystore.RoomKeyPair) error`, `TeardownRoomKeys(ctx, keys roomKeyStore, roomIDs []string) error`.
- `preset.go`: `Fixtures{Users []model.User; Rooms []model.Room; Subscriptions []model.Subscription; RoomKeys map[string]roomkeystore.RoomKeyPair}`, `deterministicRoomKeyPair(r io.Reader) roomkeystore.RoomKeyPair`.
- `pkg/subject`: `MsgSend(account, roomID, siteID string) string`, `RoomEventWildcard() string`, `MessageRead(account, roomID, siteID string) string`.
- `pkg/model`: `SendMessageRequest`, `Room`, `Subscription`, `SubscriptionUser`, `User`, `RoleOwner`, `RoleMember`, `RoomTypeChannel`.

---

## Task 1: BotRoom preset + fixtures

**Files:**
- Create: `tools/loadgen/seed_botroom.go`
- Test: `tools/loadgen/seed_botroom_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
)

func TestBuildBotRoomFixtures_Deterministic(t *testing.T) {
	p, ok := BuiltinBotRoomPreset("botroom-small")
	require.True(t, ok)

	f1, l1 := BuildBotRoomFixtures(&p, 42, "site-local")
	f2, l2 := BuildBotRoomFixtures(&p, 42, "site-local")

	assert.Equal(t, f1.Rooms, f2.Rooms)
	assert.Equal(t, f1.Subscriptions, f2.Subscriptions)
	assert.Equal(t, l1.RoomsBySize, l2.RoomsBySize)
}

func TestBuildBotRoomFixtures_Shape(t *testing.T) {
	p, ok := BuiltinBotRoomPreset("botroom-small")
	require.True(t, ok)

	f, layout := BuildBotRoomFixtures(&p, 7, "site-local")

	// One set of RoomsPerSize rooms per size.
	for _, size := range p.Sizes {
		rooms := layout.RoomsBySize[size]
		require.Len(t, rooms, p.RoomsPerSize, "size %d", size)
	}

	// Each room has exactly `size` subscriptions, the first being the bot owner.
	subsByRoom := map[string][]model.Subscription{}
	for _, s := range f.Subscriptions {
		subsByRoom[s.RoomID] = append(subsByRoom[s.RoomID], s)
	}
	for _, size := range p.Sizes {
		for _, rid := range layout.RoomsBySize[size] {
			subs := subsByRoom[rid]
			require.Len(t, subs, size, "room %s", rid)
			ownerCount := 0
			botIsOwner := false
			for _, s := range subs {
				for _, r := range s.Roles {
					if r == model.RoleOwner {
						ownerCount++
						if s.User.Account == layout.BotAccount {
							botIsOwner = true
						}
					}
				}
			}
			assert.Equal(t, 1, ownerCount, "room %s owner count", rid)
			assert.True(t, botIsOwner, "room %s bot must be owner", rid)
		}
	}

	// Every room is a channel with UserCount == size and a room key.
	roomByID := map[string]model.Room{}
	for _, r := range f.Rooms {
		roomByID[r.ID] = r
	}
	for _, size := range p.Sizes {
		for _, rid := range layout.RoomsBySize[size] {
			assert.Equal(t, model.RoomTypeChannel, roomByID[rid].Type)
			assert.Equal(t, size, roomByID[rid].UserCount)
			_, hasKey := f.RoomKeys[rid]
			assert.True(t, hasKey, "room %s must have a key", rid)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=loadgen` (or `go test ./tools/loadgen/ -run TestBuildBotRoomFixtures`)
Expected: FAIL — `undefined: BuiltinBotRoomPreset`, `BuildBotRoomFixtures`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/roomkeystore"
)

// BotRoomPreset is a fully-deterministic spec for the large-room bot workload.
// It seeds RoomsPerSize channel rooms at each size in Sizes, each owned by a
// single bot account that also acts as the message sender.
type BotRoomPreset struct {
	Name         string
	Users        int   // shared member pool (excludes the bot)
	Sizes        []int // room sizes to seed
	RoomsPerSize int   // rooms seeded per size
	BotAccount   string
}

// BotRoomLayout is the in-memory map from size to the seeded room IDs of that
// size, derived deterministically from (preset, seed). Not persisted.
type BotRoomLayout struct {
	Sizes       []int
	RoomsBySize map[int][]string
	BotAccount  string
}

var builtinBotRoomPresets = map[string]BotRoomPreset{
	"botroom-small": {
		Name: "botroom-small", Users: 300, RoomsPerSize: 4,
		Sizes: []int{50, 100, 200}, BotAccount: "bot-0",
	},
	"botroom-medium": {
		Name: "botroom-medium", Users: 5500, RoomsPerSize: 4,
		Sizes: []int{100, 500, 1000, 2000, 5000}, BotAccount: "bot-0",
	},
}

// BuiltinBotRoomPreset looks up a preset by name.
func BuiltinBotRoomPreset(name string) (BotRoomPreset, bool) {
	p, ok := builtinBotRoomPresets[name]
	return p, ok
}

// BuildBotRoomFixtures is a pure function of (preset, seed, siteID) producing
// the full botroom fixture set plus the size→roomIDs layout. Equal inputs
// produce equal outputs.
func BuildBotRoomFixtures(p *BotRoomPreset, seed int64, siteID string) (Fixtures, BotRoomLayout) {
	r := rand.New(rand.NewSource(seed))
	now := time.Unix(0, 0).UTC()

	// Bot user is index -1 conceptually; give it a stable ID/account.
	bot := model.User{
		ID:      "b-000000",
		Account: p.BotAccount,
		SiteID:  siteID,
		EngName: "bot", ChineseName: "bot",
	}
	users := make([]model.User, p.Users+1)
	users[0] = bot
	for i := 0; i < p.Users; i++ {
		users[i+1] = model.User{
			ID:      fmt.Sprintf("u-%06d", i),
			Account: fmt.Sprintf("user-%d", i),
			SiteID:  siteID,
			EngName: "member", ChineseName: "member",
		}
	}
	memberPool := users[1:]

	var rooms []model.Room
	var subs []model.Subscription
	roomKeys := make(map[string]roomkeystore.RoomKeyPair)
	layout := BotRoomLayout{
		Sizes:       append([]int(nil), p.Sizes...),
		RoomsBySize: make(map[int][]string, len(p.Sizes)),
		BotAccount:  p.BotAccount,
	}

	for _, size := range p.Sizes {
		for ri := 0; ri < p.RoomsPerSize; ri++ {
			roomID := fmt.Sprintf("broom-s%d-r%d", size, ri)
			rooms = append(rooms, model.Room{
				ID:        roomID,
				Name:      roomID,
				Type:      model.RoomTypeChannel,
				SiteID:    siteID,
				UserCount: size,
				CreatedAt: now,
				UpdatedAt: now,
			})
			layout.RoomsBySize[size] = append(layout.RoomsBySize[size], roomID)

			// Bot owner subscription first.
			subs = append(subs, model.Subscription{
				ID:       fmt.Sprintf("sub-%s-%s", roomID, bot.ID),
				User:     model.SubscriptionUser{ID: bot.ID, Account: bot.Account},
				RoomID:   roomID,
				SiteID:   siteID,
				Roles:    []model.Role{model.RoleOwner},
				JoinedAt: now,
			})
			// size-1 members drawn from the shared pool.
			perm := r.Perm(len(memberPool))
			need := size - 1
			if need > len(perm) {
				need = len(perm)
			}
			for _, idx := range perm[:need] {
				m := memberPool[idx]
				subs = append(subs, model.Subscription{
					ID:       fmt.Sprintf("sub-%s-%s", roomID, m.ID),
					User:     model.SubscriptionUser{ID: m.ID, Account: m.Account},
					RoomID:   roomID,
					SiteID:   siteID,
					Roles:    []model.Role{model.RoleMember},
					JoinedAt: now,
				})
			}
			roomKeys[roomID] = deterministicRoomKeyPair(r)
		}
	}

	return Fixtures{Users: users, Rooms: rooms, Subscriptions: subs, RoomKeys: roomKeys}, layout
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run TestBuildBotRoomFixtures -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/seed_botroom.go tools/loadgen/seed_botroom_test.go
git commit -m "feat(loadgen): botroom preset and deterministic fixtures"
```

---

## Task 2: Wire `botroom` into seed / teardown

**Files:**
- Modify: `tools/loadgen/main.go` (the `runSeed` switch ~line 140, `runTeardown` switch ~line 231)
- Create helpers in `tools/loadgen/seed_botroom.go`

- [ ] **Step 1: Write the failing test**

Add to `tools/loadgen/seed_botroom_test.go`:

```go
func TestBotRoomLayout_RoomIDs(t *testing.T) {
	p, _ := BuiltinBotRoomPreset("botroom-small")
	f, layout := BuildBotRoomFixtures(&p, 1, "site-local")

	all := botRoomRoomIDs(&layout)
	assert.Len(t, all, len(p.Sizes)*p.RoomsPerSize)
	assert.Len(t, f.Rooms, len(all))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestBotRoomLayout_RoomIDs`
Expected: FAIL — `undefined: botRoomRoomIDs`.

- [ ] **Step 3: Write minimal implementation**

Add to `tools/loadgen/seed_botroom.go`:

```go
// botRoomRoomIDs flattens the layout's per-size room IDs into one slice.
func botRoomRoomIDs(l *BotRoomLayout) []string {
	var out []string
	for _, size := range l.Sizes {
		out = append(out, l.RoomsBySize[size]...)
	}
	return out
}
```

Add to `tools/loadgen/main.go` — a seed runner (place next to `runSeedMembers`):

```go
func runSeedBotRoom(ctx context.Context, cfg *config, preset string, seed int64) int {
	p, ok := BuiltinBotRoomPreset(preset)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown botroom preset: %s\n", preset)
		return 2
	}
	db, keyStore, cleanup, err := connectStores(ctx, cfg)
	if err != nil {
		return 1
	}
	defer cleanup()
	fixtures, layout := BuildBotRoomFixtures(&p, seed, cfg.SiteID)
	if err := Seed(ctx, db, &fixtures); err != nil {
		slog.Error("seed", "error", err)
		return 1
	}
	if err := SeedRoomKeys(ctx, keyStore, fixtures.RoomKeys); err != nil {
		slog.Error("seed room keys", "error", err)
		return 1
	}
	slog.Info("seed complete (botroom)",
		"preset", p.Name, "users", len(fixtures.Users),
		"rooms", len(fixtures.Rooms), "subs", len(fixtures.Subscriptions),
		"sizes", layout.Sizes)
	return 0
}

func runTeardownBotRoom(ctx context.Context, cfg *config, preset string, seed int64) int {
	p, ok := BuiltinBotRoomPreset(preset)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown botroom preset: %s\n", preset)
		return 2
	}
	db, keyStore, cleanup, err := connectStores(ctx, cfg)
	if err != nil {
		return 1
	}
	defer cleanup()
	fixtures, layout := BuildBotRoomFixtures(&p, seed, cfg.SiteID)
	_ = fixtures
	if err := Teardown(ctx, db); err != nil {
		slog.Error("teardown", "error", err)
		return 1
	}
	if err := TeardownRoomKeys(ctx, keyStore, botRoomRoomIDs(&layout)); err != nil {
		slog.Error("teardown room keys", "error", err)
		return 1
	}
	slog.Info("teardown complete (botroom)", "preset", p.Name)
	return 0
}
```

In `runSeed`, extend the `--workload` doc string and switch:

```go
	workload := fs.String("workload", "messages", "messages|members|history|room-read|botroom")
```
```go
	case "botroom":
		return runSeedBotRoom(ctx, cfg, *preset, *seed)
```

In `runTeardown`, mirror it:

```go
	workload := fs.String("workload", "messages", "messages|members|history|room-read|botroom")
```
```go
	case "botroom":
		return runTeardownBotRoom(ctx, cfg, *preset, *seed)
```

- [ ] **Step 4: Run test + build**

Run: `go test ./tools/loadgen/ -run TestBotRoomLayout_RoomIDs && go build ./tools/loadgen/`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/main.go tools/loadgen/seed_botroom.go tools/loadgen/seed_botroom_test.go
git commit -m "feat(loadgen): seed/teardown wiring for botroom workload"
```

---

## Task 3: BotRoom metrics

**Files:**
- Modify: `tools/loadgen/metrics.go` (`Metrics` struct + `NewMetrics`)
- Test: `tools/loadgen/metrics_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Add to `tools/loadgen/metrics_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewMetrics_BotRoomFamiliesRegistered(t *testing.T) {
	m := NewMetrics()
	m.BotRoomPublished.WithLabelValues("botroom-small", "measured", "100").Inc()
	m.BotRoomPublishErrors.WithLabelValues("publish").Inc()
	m.BotRoomE2ELatency.WithLabelValues("100").Observe(0.01)
	m.BotRoomReadLatency.WithLabelValues("100").Observe(0.01)

	mfs, err := m.Registry.Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	require.True(t, names["loadgen_botroom_published_total"])
	require.True(t, names["loadgen_botroom_publish_errors_total"])
	require.True(t, names["loadgen_botroom_e2e_latency_seconds"])
	require.True(t, names["loadgen_botroom_read_latency_seconds"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestNewMetrics_BotRoom`
Expected: FAIL — `m.BotRoomPublished undefined`.

- [ ] **Step 3: Write minimal implementation**

In `tools/loadgen/metrics.go`, add fields to the `Metrics` struct:

```go
	BotRoomPublished     *prometheus.CounterVec
	BotRoomPublishErrors *prometheus.CounterVec
	BotRoomE2ELatency    *prometheus.HistogramVec
	BotRoomReadLatency   *prometheus.HistogramVec
```

In `NewMetrics`, after the member metrics are built and before `r.MustRegister(...)`, construct them (reuse the existing `buckets` variable):

```go
	m.BotRoomPublished = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "loadgen_botroom_published_total", Help: "Bot messages published by preset/phase/size."},
		[]string{"preset", "phase", "size"},
	)
	m.BotRoomPublishErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "loadgen_botroom_publish_errors_total", Help: "Bot publish errors by reason (publish|gatekeeper|timeout|saturated|underrun)."},
		[]string{"reason"},
	)
	m.BotRoomE2ELatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "loadgen_botroom_e2e_latency_seconds", Help: "Publish→broadcast latency by room size.", Buckets: buckets},
		[]string{"size"},
	)
	m.BotRoomReadLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "loadgen_botroom_read_latency_seconds", Help: "room-service read latency by room size.", Buckets: buckets},
		[]string{"size"},
	)
```

Add them to the existing `r.MustRegister(...)` call:

```go
	r.MustRegister(
		m.Published, m.PublishErrors,
		m.E1Latency, m.E2Latency,
		m.ConsumerPending, m.ConsumerAckPending, m.ConsumerRedelivered,
		m.MemberPublished, m.MemberPublishErrors,
		m.MemberE1Latency, m.MemberE2Latency, m.MemberRoomSize,
		m.BotRoomPublished, m.BotRoomPublishErrors,
		m.BotRoomE2ELatency, m.BotRoomReadLatency,
	)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run TestNewMetrics_BotRoom -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/metrics.go tools/loadgen/metrics_test.go
git commit -m "feat(loadgen): botroom prometheus metric families"
```

---

## Task 4: BotRoom paced sender

**Files:**
- Create: `tools/loadgen/botroom.go`
- Test: `tools/loadgen/botroom_test.go`

The sender publishes `SendMessageRequest` as the bot owner to `subject.MsgSend(bot, roomID, siteID)`, round-robin across the step's rooms, and records publish times in the shared `Collector` for E2E correlation. It tracks live `attempted`/`failed` counters so the verdict can window per-step.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubBotPublisher struct {
	mu    sync.Mutex
	subjs []string
	fail  bool
}

func (s *stubBotPublisher) Publish(_ context.Context, subject string, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subjs = append(s.subjs, subject)
	if s.fail {
		return assertErr
	}
	return nil
}

var assertErr = &stubErr{}

type stubErr struct{}

func (*stubErr) Error() string { return "stub publish error" }

func TestBotRoomSender_RoundRobinAcrossRooms(t *testing.T) {
	m := NewMetrics()
	c := NewCollector(m, "botroom-small")
	pub := &stubBotPublisher{}
	rooms := []string{"broom-s100-r0", "broom-s100-r1", "broom-s100-r2"}

	s := NewBotRoomSender(&BotRoomSenderConfig{
		Publisher: pub, Collector: c, Metrics: m,
		SiteID: "site-local", BotAccount: "bot-0",
		Rooms: rooms, Size: 100, Rate: 200, MaxInFlight: 50,
		Content: "x",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	require.Positive(t, s.Attempted())
	// Every targeted room should have been hit at least once.
	pub.mu.Lock()
	defer pub.mu.Unlock()
	seen := map[string]bool{}
	for _, subj := range pub.subjs {
		seen[subj] = true
	}
	for _, rid := range rooms {
		want := "chat.user.bot-0.room." + rid + ".site-local.msg.send"
		assert.True(t, seen[want], "expected a publish to %s", want)
	}
}

func TestBotRoomSender_CountsPublishFailures(t *testing.T) {
	m := NewMetrics()
	c := NewCollector(m, "botroom-small")
	pub := &stubBotPublisher{fail: true}

	s := NewBotRoomSender(&BotRoomSenderConfig{
		Publisher: pub, Collector: c, Metrics: m,
		SiteID: "site-local", BotAccount: "bot-0",
		Rooms: []string{"broom-s100-r0"}, Size: 100, Rate: 200, MaxInFlight: 50,
		Content: "x",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	assert.Positive(t, s.Failed())
	assert.Equal(t, s.Attempted(), s.Failed(), "every attempt failed")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestBotRoomSender`
Expected: FAIL — `undefined: NewBotRoomSender`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// BotRoomSenderConfig parameterizes a single-step bot sender.
type BotRoomSenderConfig struct {
	Publisher   Publisher
	Collector   *Collector
	Metrics     *Metrics
	Preset      string
	SiteID      string
	BotAccount  string
	Rooms       []string // rooms for this size step
	Size        int
	Rate        int
	MaxInFlight int
	Content     string
}

// BotRoomSender drives a paced open-loop publish across the step's rooms.
// Attempted/Failed are cumulative across the sender's lifetime; the ramp loop
// snapshots them at hold start/end to window per-step.
type BotRoomSender struct {
	cfg       BotRoomSenderConfig
	sizeLabel string
	cursor    atomic.Uint64
	attempted atomic.Int64
	failed    atomic.Int64
}

// NewBotRoomSender constructs a sender. Content defaults to a 200-byte filler
// when empty.
func NewBotRoomSender(cfg *BotRoomSenderConfig) *BotRoomSender {
	if cfg.Content == "" {
		buf := make([]byte, 200)
		for i := range buf {
			buf[i] = 'x'
		}
		cfg.Content = string(buf)
	}
	return &BotRoomSender{cfg: *cfg, sizeLabel: strconv.Itoa(cfg.Size)}
}

// Attempted returns the number of measured publish attempts so far.
func (s *BotRoomSender) Attempted() int64 { return s.attempted.Load() }

// Failed returns the number of publish failures so far.
func (s *BotRoomSender) Failed() int64 { return s.failed.Load() }

// Run paces publishes until ctx is cancelled.
func (s *BotRoomSender) Run(ctx context.Context) {
	if s.cfg.Rate <= 0 || len(s.cfg.Rooms) == 0 {
		return
	}
	maxInFlight := s.cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 1
	}
	pubErrs := s.cfg.Metrics.BotRoomPublishErrors
	pacedDispatch(ctx, s.cfg.Rate, maxInFlight,
		func(n int) {
			if n > 0 {
				pubErrs.WithLabelValues("underrun").Add(float64(n))
			}
		},
		func() { pubErrs.WithLabelValues("saturated").Inc() },
		s.publishOne,
	)
}

func (s *BotRoomSender) publishOne(ctx context.Context) {
	idx := s.cursor.Add(1) - 1
	roomID := s.cfg.Rooms[int(idx)%len(s.cfg.Rooms)]

	msgID := idgen.GenerateMessageID()
	reqID := idgen.GenerateRequestID()
	req := model.SendMessageRequest{ID: msgID, Content: s.cfg.Content, RequestID: reqID}
	data, err := json.Marshal(req)
	if err != nil {
		s.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("publish").Inc()
		return
	}
	now := time.Now()
	s.cfg.Collector.RecordPublish(reqID, msgID, now)
	s.attempted.Add(1)

	subj := subject.MsgSend(s.cfg.BotAccount, roomID, s.cfg.SiteID)
	if perr := s.cfg.Publisher.Publish(ctx, subj, data); perr != nil {
		s.cfg.Collector.RecordPublishFailed(reqID, msgID)
		s.failed.Add(1)
		s.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("publish").Inc()
		return
	}
	// BotRoomPublished labels are []string{"preset", "phase", "size"}.
	s.cfg.Metrics.BotRoomPublished.WithLabelValues(s.cfg.Preset, "measured", s.sizeLabel).Inc()
}
```

`Preset` is carried on `BotRoomSenderConfig` and set by the ramp loop (Task 7,
`Preset: c.Preset`). The Task 4 tests leave it empty (the metric label is not
asserted there), which is fine.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run TestBotRoomSender -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/botroom.go tools/loadgen/botroom_test.go
git commit -m "feat(loadgen): botroom paced bot sender"
```

---

## Task 5: BotRoom verdict

**Files:**
- Create: `tools/loadgen/botroom_verdict.go`
- Test: `tools/loadgen/botroom_verdict_test.go`

Reuses `ConsumerPendingDelta` and `SelfMetrics` from `daily_verdict.go`. The critical difference from daily: **no notification-worker exemption** — all three message-pipeline durables are gated.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func defaultBotRoomThresholdsForTest() BotRoomThresholds {
	return BotRoomThresholds{
		P95LatencyMs: 500, P99LatencyMs: 1000,
		ErrorRate: 0.001, PendingGrowth: 1000,
		GCPauseInconclusive: 50, RateTolerance: 0.05,
	}
}

func TestEvaluateBotRoomStep_PassWhenHealthy(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(10, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{"notification-worker": {Delta: 50}},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.False(t, r.Tripped)
	assert.False(t, r.Inconclusive)
}

func TestEvaluateBotRoomStep_TripsOnNotificationBacklog(t *testing.T) {
	in := botRoomStepInputs{
		Size: 5000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(20, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{"notification-worker": {Delta: 9000}},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Tripped)
	assert.Contains(t, r.TrippedReasons[0], "notification-worker")
}

func TestEvaluateBotRoomStep_TripsOnLatency(t *testing.T) {
	in := botRoomStepInputs{
		Size: 5000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(900, 6000), // p95/p99 ~900ms > 500/1000? p95 trips
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Tripped)
}

func TestEvaluateBotRoomStep_InconclusiveOnRateShortfall(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 3000, Failed: 0, // 100/s achieved vs 200 target → shortfall
		E2Samples:       msSamples(10, 3000),
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 5},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Inconclusive)
}

func TestEvaluateBotRoomStep_InconclusiveOnGCPause(t *testing.T) {
	in := botRoomStepInputs{
		Size: 1000, Rooms: 4, TargetRate: 200, HoldSeconds: 30,
		Attempted: 6000, Failed: 0,
		E2Samples:       msSamples(10, 6000),
		ConsumerPending: map[string]ConsumerPendingDelta{},
		Self:            SelfMetrics{GCPauseP99Ms: 99},
	}
	r := evaluateBotRoomStep(in, defaultBotRoomThresholdsForTest())
	assert.True(t, r.Inconclusive)
}

// msSamples returns n samples each of approximately `ms` milliseconds.
func msSamples(ms float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = ms
	}
	return out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestEvaluateBotRoomStep`
Expected: FAIL — `undefined: BotRoomThresholds`, `botRoomStepInputs`, `evaluateBotRoomStep`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"fmt"
	"sort"
)

// botRoomGatedDurables are the message-pipeline consumers whose backlog growth
// fails a step. Unlike the daily scenario, notification-worker is NOT exempt —
// it is the headline O(N)-per-message signal this scenario hunts.
var botRoomGatedDurables = []string{"message-worker", "broadcast-worker", "notification-worker"}

// BotRoomThresholds gates a botroom step.
type BotRoomThresholds struct {
	P95LatencyMs        float64
	P99LatencyMs        float64
	ErrorRate           float64
	PendingGrowth       int64
	GCPauseInconclusive float64
	RateTolerance       float64
}

// botRoomStepInputs is the raw measurement bundle for one size step.
type botRoomStepInputs struct {
	Size        int
	Rooms       int
	TargetRate  int
	HoldSeconds float64
	Attempted   int64
	Failed      int64
	E2Samples   []float64 // publish→broadcast latency, milliseconds
	ReadSamples []float64 // room-service read latency, milliseconds (nil when reads off)
	ConsumerPending map[string]ConsumerPendingDelta
	Self        SelfMetrics
}

// BotRoomStepResult is the evaluated verdict for one size step.
type BotRoomStepResult struct {
	Size           int
	Rooms          int
	TargetRate     int
	AchievedRate   float64
	E2P50Ms        float64
	E2P95Ms        float64
	E2P99Ms        float64
	ReadP95Ms      float64
	ReadP99Ms      float64
	ErrorRate      float64
	Attempted      int64
	Failed         int64
	ConsumerPending map[string]ConsumerPendingDelta
	Tripped        bool
	Inconclusive   bool
	TrippedReasons []string
}

func evaluateBotRoomStep(in botRoomStepInputs, th BotRoomThresholds) BotRoomStepResult {
	r := BotRoomStepResult{
		Size: in.Size, Rooms: in.Rooms, TargetRate: in.TargetRate,
		Attempted: in.Attempted, Failed: in.Failed,
		ConsumerPending: in.ConsumerPending,
	}
	if in.HoldSeconds > 0 {
		r.AchievedRate = float64(in.Attempted) / in.HoldSeconds
	}
	if in.Attempted > 0 {
		r.ErrorRate = float64(in.Failed) / float64(in.Attempted)
	}
	r.E2P50Ms = percentileMs(in.E2Samples, 0.50)
	r.E2P95Ms = percentileMs(in.E2Samples, 0.95)
	r.E2P99Ms = percentileMs(in.E2Samples, 0.99)
	r.ReadP95Ms = percentileMs(in.ReadSamples, 0.95)
	r.ReadP99Ms = percentileMs(in.ReadSamples, 0.99)

	// INCONCLUSIVE overrides everything: the verdict signals can't be trusted.
	if in.Self.GCPauseP99Ms > th.GCPauseInconclusive {
		r.Inconclusive = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("inconclusive: gc=%.1fms > %.0fms", in.Self.GCPauseP99Ms, th.GCPauseInconclusive))
		return r
	}
	if in.TargetRate > 0 && r.AchievedRate < float64(in.TargetRate)*(1-th.RateTolerance) {
		r.Inconclusive = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("inconclusive: achieved %.0f/s < %d/s target (load box limited)", r.AchievedRate, in.TargetRate))
		return r
	}

	// TRIP signals.
	if r.E2P95Ms > th.P95LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("E2E p95=%.0fms > %.0fms", r.E2P95Ms, th.P95LatencyMs))
	}
	if r.E2P99Ms > th.P99LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("E2E p99=%.0fms > %.0fms", r.E2P99Ms, th.P99LatencyMs))
	}
	if len(in.ReadSamples) > 0 {
		if r.ReadP95Ms > th.P95LatencyMs {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("read p95=%.0fms > %.0fms", r.ReadP95Ms, th.P95LatencyMs))
		}
		if r.ReadP99Ms > th.P99LatencyMs {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("read p99=%.0fms > %.0fms", r.ReadP99Ms, th.P99LatencyMs))
		}
	}
	if in.Attempted > 0 && r.ErrorRate > th.ErrorRate {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("error rate %.4f > %.4f", r.ErrorRate, th.ErrorRate))
	}
	for _, durable := range botRoomGatedDurables {
		d, ok := in.ConsumerPending[durable]
		if !ok {
			continue
		}
		switch {
		case d.Delta > th.PendingGrowth:
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s pending +%d > +%d", durable, d.Delta, th.PendingGrowth))
		case d.End == 0 && d.Start > 0:
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s disappeared mid-hold (had %d pending at start)", durable, d.Start))
		}
	}
	return r
}

// percentileMs returns the q-quantile of samples (already in ms). Empty → 0.
func percentileMs(samples []float64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * q)
	return sorted[idx]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run TestEvaluateBotRoomStep -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/botroom_verdict.go tools/loadgen/botroom_verdict_test.go
git commit -m "feat(loadgen): botroom per-step SLO verdict (notification-worker gated)"
```

---

## Task 6: BotRoom report (table + ANSWER + CSV)

**Files:**
- Create: `tools/loadgen/botroom_report.go`
- Test: `tools/loadgen/botroom_report_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderBotRoomConsole_AnswerIsLargestPassingSize(t *testing.T) {
	results := []BotRoomStepResult{
		{Size: 100, E2P95Ms: 20, Tripped: false},
		{Size: 500, E2P95Ms: 60, Tripped: false},
		{Size: 1000, E2P95Ms: 800, Tripped: true, TrippedReasons: []string{"E2E p95=800ms > 500ms"}},
	}
	var buf bytes.Buffer
	renderBotRoomConsole(&buf, results)
	out := buf.String()
	assert.Contains(t, out, "ANSWER: max room size = 500")
	assert.Contains(t, out, "Next limit: E2E p95=800ms > 500ms")
}

func TestRenderBotRoomConsole_NoStepPassed(t *testing.T) {
	results := []BotRoomStepResult{
		{Size: 100, Tripped: true, TrippedReasons: []string{"x"}},
	}
	var buf bytes.Buffer
	renderBotRoomConsole(&buf, results)
	assert.Contains(t, buf.String(), "ANSWER: no step passed")
}

func TestWriteBotRoomCSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	results := []BotRoomStepResult{
		{Size: 100, Rooms: 4, TargetRate: 200, AchievedRate: 199, E2P95Ms: 20, Tripped: false},
	}
	require.NoError(t, writeBotRoomCSV(path, results))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	assert.Equal(t, "size,rooms,target_rate,achieved_rate,e2_p50_ms,e2_p95_ms,e2_p99_ms,read_p95_ms,read_p99_ms,error_rate,attempted,failed,worst_durable,worst_pending_delta,tripped,inconclusive,reasons", lines[0])
	assert.Len(t, lines, 2)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run 'TestRenderBotRoomConsole|TestWriteBotRoomCSV'`
Expected: FAIL — `undefined: renderBotRoomConsole`, `writeBotRoomCSV`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func renderBotRoomConsole(w io.Writer, results []BotRoomStepResult) {
	fmt.Fprintln(w, "size     rooms  rate    e2_p50  e2_p95  e2_p99  err%    worst-pending          verdict")
	for _, r := range results {
		verdict := "PASS"
		if r.Inconclusive {
			verdict = "INCONCLUSIVE"
		} else if r.Tripped {
			verdict = "TRIP"
		}
		fmt.Fprintf(w, "%-8d %-6d %-7d %-7.0f %-7.0f %-7.0f %-7.3f %-22s %s\n",
			r.Size, r.Rooms, r.TargetRate, r.E2P50Ms, r.E2P95Ms, r.E2P99Ms,
			r.ErrorRate*100, worstBotRoomPending(r.ConsumerPending), verdict)
		if len(r.TrippedReasons) > 0 {
			fmt.Fprintf(w, "    reasons: %s\n", strings.Join(r.TrippedReasons, "; "))
		}
	}

	best := -1
	var firstTrip *BotRoomStepResult
	for i := range results {
		if !results[i].Tripped && !results[i].Inconclusive {
			if results[i].Size > best {
				best = results[i].Size
			}
		}
		if results[i].Tripped && firstTrip == nil {
			firstTrip = &results[i]
		}
	}
	if best < 0 {
		fmt.Fprintln(w, "\nANSWER: no step passed")
		return
	}
	fmt.Fprintf(w, "\nANSWER: max room size = %d\n", best)
	if firstTrip != nil && len(firstTrip.TrippedReasons) > 0 {
		fmt.Fprintf(w, "        Next limit: %s\n", firstTrip.TrippedReasons[0])
	}
}

func worstBotRoomPending(m map[string]ConsumerPendingDelta) string {
	worst := ""
	var worstDelta int64
	for durable, d := range m {
		if worst == "" || d.Delta > worstDelta {
			worst = durable
			worstDelta = d.Delta
		}
	}
	if worst == "" {
		return "-"
	}
	return fmt.Sprintf("%s +%d", worst, worstDelta)
}

func writeBotRoomCSV(path string, results []BotRoomStepResult) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer func() { _ = f.Close() }()
	cw := csv.NewWriter(f)
	defer cw.Flush()
	header := []string{
		"size", "rooms", "target_rate", "achieved_rate",
		"e2_p50_ms", "e2_p95_ms", "e2_p99_ms", "read_p95_ms", "read_p99_ms",
		"error_rate", "attempted", "failed",
		"worst_durable", "worst_pending_delta", "tripped", "inconclusive", "reasons",
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, r := range results {
		wd, wdelta := worstDurable(r.ConsumerPending)
		row := []string{
			strconv.Itoa(r.Size), strconv.Itoa(r.Rooms), strconv.Itoa(r.TargetRate),
			strconv.FormatFloat(r.AchievedRate, 'f', 1, 64),
			strconv.FormatFloat(r.E2P50Ms, 'f', 1, 64),
			strconv.FormatFloat(r.E2P95Ms, 'f', 1, 64),
			strconv.FormatFloat(r.E2P99Ms, 'f', 1, 64),
			strconv.FormatFloat(r.ReadP95Ms, 'f', 1, 64),
			strconv.FormatFloat(r.ReadP99Ms, 'f', 1, 64),
			strconv.FormatFloat(r.ErrorRate, 'f', 6, 64),
			strconv.FormatInt(r.Attempted, 10), strconv.FormatInt(r.Failed, 10),
			wd, strconv.FormatInt(wdelta, 10),
			strconv.FormatBool(r.Tripped), strconv.FormatBool(r.Inconclusive),
			strings.Join(r.TrippedReasons, " | "),
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	return nil
}

func worstDurable(m map[string]ConsumerPendingDelta) (string, int64) {
	worst := ""
	var worstDelta int64
	for durable, d := range m {
		if worst == "" || d.Delta > worstDelta {
			worst = durable
			worstDelta = d.Delta
		}
	}
	return worst, worstDelta
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run 'TestRenderBotRoomConsole|TestWriteBotRoomCSV' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/botroom_report.go tools/loadgen/botroom_report_test.go
git commit -m "feat(loadgen): botroom report table, ANSWER line and CSV"
```

---

## Task 7: `max-room-size` subcommand (ramp wiring)

**Files:**
- Modify: `tools/loadgen/main.go` (dispatch switch + new `runMaxRoomSize`)
- Modify: `tools/loadgen/botroom.go` (add `Preset` field per Task 4 note; add `botRoomSubset` preflight helper)
- Test: `tools/loadgen/botroom_test.go` (preflight test)

- [ ] **Step 1: Write the failing test**

Add to `tools/loadgen/botroom_test.go`:

```go
func TestValidateBotRoomSelection(t *testing.T) {
	p, _ := BuiltinBotRoomPreset("botroom-small") // sizes 50,100,200; rooms/size 4

	// OK: subset of seeded sizes, rooms within seeded count.
	require.NoError(t, ValidateBotRoomSelection(&p, []int{50, 100}, 2))

	// Bad: size not seeded.
	err := ValidateBotRoomSelection(&p, []int{50, 999}, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "999")

	// Bad: too many rooms-per-size.
	err = ValidateBotRoomSelection(&p, []int{50}, 99)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rooms-per-size")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestValidateBotRoomSelection`
Expected: FAIL — `undefined: ValidateBotRoomSelection`.

- [ ] **Step 3: Write minimal implementation**

Add to `tools/loadgen/botroom.go`:

```go
// ValidateBotRoomSelection fails fast (before any NATS/store work) when the
// requested --sizes / --rooms-per-size exceed what the preset seeded.
func ValidateBotRoomSelection(p *BotRoomPreset, sizes []int, roomsPerSize int) error {
	if roomsPerSize < 1 {
		return fmt.Errorf("--rooms-per-size must be >= 1")
	}
	if roomsPerSize > p.RoomsPerSize {
		return fmt.Errorf("--rooms-per-size=%d exceeds preset %q seeded rooms-per-size=%d",
			roomsPerSize, p.Name, p.RoomsPerSize)
	}
	seeded := make(map[int]bool, len(p.Sizes))
	for _, s := range p.Sizes {
		seeded[s] = true
	}
	for _, s := range sizes {
		if !seeded[s] {
			return fmt.Errorf("--sizes contains %d which preset %q did not seed (seeded: %v)",
				s, p.Name, p.Sizes)
		}
	}
	return nil
}
```

> Add `import "fmt"` to `botroom.go` if not already present (it is, from Task 4? No — Task 4's botroom.go imports do not include `fmt`. Add it.)

Add the dispatch case in `main.go`'s `dispatch` switch (after `case "daily":`):

```go
	case "max-room-size":
		return runMaxRoomSize(ctx, cfg, os.Args[2:])
```

Update the usage string near the top of `main`:

```go
		fmt.Fprintln(os.Stderr, "usage: loadgen <seed|run|teardown|members-sustained|members-capacity|history-sustained|max-rps|daily|max-room-size> [flags]")
```

Add `runMaxRoomSize` to `main.go`:

```go
func runMaxRoomSize(ctx context.Context, cfg *config, args []string) int {
	fs := flag.NewFlagSet("max-room-size", flag.ExitOnError)
	preset := fs.String("preset", "", "botroom preset name")
	seed := fs.Int64("seed", 42, "RNG seed (must match seed-time)")
	rate := fs.Int("rate", 0, "bot send rate (msgs/sec, split across the step's rooms) — required")
	sizesFlag := fs.String("sizes", "100,500,1000,2000,5000", "comma-separated room sizes to ramp; k suffix = x1000")
	roomsPerSize := fs.Int("rooms-per-size", 4, "rooms targeted per size step")
	reads := fs.Int("reads", 0, "optional room-service read rate (msgs/sec); 0 = off")
	warmup := fs.Duration("warmup", 60*time.Second, "per-step warmup (samples discarded)")
	hold := fs.Duration("hold", 180*time.Second, "per-step measurement window")
	cooldown := fs.Duration("cooldown", 30*time.Second, "per-step drain gap")
	stopOnTrip := fs.Bool("stop-on-trip", true, "stop the ramp at the first TRIP")
	sloP95 := fs.Duration("slo-p95", 500*time.Millisecond, "p95 latency SLO")
	sloP99 := fs.Duration("slo-p99", 1000*time.Millisecond, "p99 latency SLO")
	sloErr := fs.Float64("slo-error-rate", 0.001, "error-rate SLO (failed/attempted)")
	sloPending := fs.Int64("slo-pending-growth", 1000, "per-durable num_pending growth SLO")
	rateTol := fs.Float64("rate-tolerance", 0.05, "achieved-vs-target shortfall band for INCONCLUSIVE")
	csvPath := fs.String("csv", "", "optional CSV output path")
	_ = fs.Parse(args)

	if *preset == "" {
		fmt.Fprintln(os.Stderr, "--preset required")
		return 2
	}
	if *rate <= 0 {
		fmt.Fprintln(os.Stderr, "--rate required and must be > 0")
		return 2
	}
	p, ok := BuiltinBotRoomPreset(*preset)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown botroom preset: %s\n", *preset)
		return 2
	}
	sizes, err := parseStepList(*sizesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse --sizes: %v\n", err)
		return 2
	}
	if err := ValidateBotRoomSelection(&p, sizes, *roomsPerSize); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}

	_, layout := BuildBotRoomFixtures(&p, *seed, cfg.SiteID)

	nc, err := natsutil.Connect(cfg.NatsURL, cfg.NatsCredsFile)
	if err != nil {
		slog.Error("nats connect", "error", err)
		return 1
	}
	js, err := jetstream.New(nc.NatsConn())
	if err != nil {
		slog.Error("jetstream init", "error", err)
		return 1
	}

	metrics := NewMetrics()
	metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: metrics.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("metrics server stopped", "error", err)
		}
	}()

	collector := NewCollector(metrics, p.Name)
	// E2 (broadcast) correlation via RoomEvent.LastMsgID — reuse newE2Handler.
	e2Sub, err := nc.NatsConn().Subscribe(subject.RoomEventWildcard(), newE2Handler(collector))
	if err != nil {
		slog.Error("subscribe e2", "error", err)
		return 1
	}
	defer func() { _ = e2Sub.Unsubscribe() }()

	// E1 (gatekeeper reply) — count gatekeeper rejections as failed sends.
	gkFailed := &atomicInt64{}
	e1Sub, err := nc.NatsConn().Subscribe(subject.UserResponseWildcard(), func(msg *nats.Msg) {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(msg.Data, &payload) == nil && payload.Error != "" {
			metrics.BotRoomPublishErrors.WithLabelValues("gatekeeper").Inc()
			gkFailed.Add(1)
		}
	})
	if err != nil {
		slog.Error("subscribe e1", "error", err)
		return 1
	}
	defer func() { _ = e1Sub.Unsubscribe() }()

	// Observability samplers (verdict uses pollPending, these feed Grafana).
	canonical := stream.MessagesCanonical(cfg.SiteID)
	samplerCtx, cancelSamplers := context.WithCancel(ctx)
	defer cancelSamplers()
	for _, d := range botRoomGatedDurables {
		s := NewConsumerSampler(js, canonical.Name, d, metrics, time.Second)
		go s.Run(samplerCtx)
	}

	publisher := newNatsCorePublisher(nc.NatsConn(), InjectFrontdoor, js)
	th := BotRoomThresholds{
		P95LatencyMs: float64(sloP95.Milliseconds()), P99LatencyMs: float64(sloP99.Milliseconds()),
		ErrorRate: *sloErr, PendingGrowth: *sloPending,
		GCPauseInconclusive: 50, RateTolerance: *rateTol,
	}

	var results []BotRoomStepResult
	for _, size := range sizes {
		if ctx.Err() != nil {
			break
		}
		rooms := layout.RoomsBySize[size][:*roomsPerSize]
		res := runBotRoomStep(ctx, botRoomStepConfig{
			Preset: p.Name, SiteID: cfg.SiteID, BotAccount: layout.BotAccount,
			Size: size, Rooms: rooms, Rate: *rate, Reads: *reads,
			Warmup: *warmup, Hold: *hold, Cooldown: *cooldown,
			MaxInFlight: cfg.MaxInFlight, JSZURL: cfg.NatsMonitoringURL,
			Publisher: publisher, NC: nc.NatsConn(), Collector: collector,
			Metrics: metrics, GKFailed: gkFailed, Thresholds: th,
		})
		results = append(results, res)
		if *stopOnTrip && res.Tripped {
			break
		}
	}

	cancelSamplers()
	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	_ = metricsSrv.Shutdown(shutCtx)
	cancelShut()
	_ = nc.Drain()

	renderBotRoomConsole(os.Stdout, results)
	if *csvPath != "" {
		if err := writeBotRoomCSV(*csvPath, results); err != nil {
			slog.Error("csv export", "error", err)
		}
	}
	return 0
}

// atomicInt64 is a tiny wrapper so the e1 handler and the step loop share a
// counter without importing sync/atomic into main.go's already-large surface.
type atomicInt64 struct{ v atomic.Int64 }

func (a *atomicInt64) Add(n int64) { a.v.Add(n) }
func (a *atomicInt64) Load() int64 { return a.v.Load() }
```

> Add `"sync/atomic"` to `main.go` imports.

Add the per-step driver to `tools/loadgen/botroom.go`:

```go
// botRoomStepConfig bundles everything one size step needs.
type botRoomStepConfig struct {
	Preset      string
	SiteID      string
	BotAccount  string
	Size        int
	Rooms       []string
	Rate        int
	Reads       int
	Warmup      time.Duration
	Hold        time.Duration
	Cooldown    time.Duration
	MaxInFlight int
	JSZURL      string
	Publisher   Publisher
	NC          *nats.Conn
	Collector   *Collector
	Metrics     *Metrics
	GKFailed    *atomicInt64
	Thresholds  BotRoomThresholds
}

// runBotRoomStep warms up, holds (polling pending at start/end), evaluates the
// verdict, then cools down. The shared collector is windowed to the hold via
// DiscardBefore(holdStart).
func runBotRoomStep(ctx context.Context, c botRoomStepConfig) BotRoomStepResult {
	sender := NewBotRoomSender(&BotRoomSenderConfig{
		Publisher: c.Publisher, Collector: c.Collector, Metrics: c.Metrics,
		SiteID: c.SiteID, BotAccount: c.BotAccount, Rooms: c.Rooms,
		Size: c.Size, Rate: c.Rate, MaxInFlight: c.MaxInFlight, Preset: c.Preset,
	})

	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sender.Run(stepCtx) }()

	var reader *botRoomReader
	if c.Reads > 0 {
		reader = newBotRoomReader(c.NC, c.SiteID, c.BotAccount, c.Rooms, c.Reads, sender)
		wg.Add(1)
		go func() { defer wg.Done(); reader.Run(stepCtx) }()
	}

	waitOrDone(ctx, c.Warmup)

	// Hold window starts now: drop warmup samples and snapshot baselines.
	holdStart := time.Now()
	c.Collector.DiscardBefore(holdStart)
	a0, f0 := sender.Attempted(), sender.Failed()+c.GKFailed.Load()
	startPending, _ := pollPending(ctx, c.JSZURL)

	waitOrDone(ctx, c.Hold)

	endPending, _ := pollPending(ctx, c.JSZURL)
	a1, f1 := sender.Attempted(), sender.Failed()+c.GKFailed.Load()

	// Settle briefly so trailing broadcasts land before the snapshot.
	time.Sleep(time.Second)

	in := botRoomStepInputs{
		Size: c.Size, Rooms: len(c.Rooms), TargetRate: c.Rate,
		HoldSeconds: c.Hold.Seconds(),
		Attempted:   a1 - a0,
		Failed:      f1 - f0,
		E2Samples:   durationsToMs(c.Collector.E2Samples()),
		Self:        snapshotSelfMetrics(),
	}
	if reader != nil {
		in.ReadSamples = reader.SamplesMs()
	}
	if startPending != nil && endPending != nil {
		in.ConsumerPending = diffPending(startPending, endPending)
	}

	cancel() // stop sender/reader before cooldown
	wg.Wait()
	waitOrDone(ctx, c.Cooldown)

	return evaluateBotRoomStep(in, c.Thresholds)
}

// waitOrDone blocks for d or until ctx is cancelled, whichever comes first.
func waitOrDone(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// durationsToMs converts a latency slice to float64 milliseconds.
func durationsToMs(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = float64(d.Microseconds()) / 1000.0
	}
	return out
}
```

> Add imports to `botroom.go`: `"context"`, `"sync"`, `"time"`, `"github.com/nats-io/nats.go"`. (`encoding/json`, `strconv`, `sync/atomic` already added in Task 4; `fmt` added above.)

The read driver (`botRoomReader`) is implemented in Task 8 — until then, guard the `c.Reads > 0` branch behind a stub that the next task replaces. For Task 7, add a temporary stub so the package compiles:

```go
// botRoomReader is implemented in Task 8. Stub kept minimal so Task 7 compiles.
type botRoomReader struct{}

func newBotRoomReader(_ *nats.Conn, _ , _ string, _ []string, _ int, _ *BotRoomSender) *botRoomReader {
	return &botRoomReader{}
}
func (r *botRoomReader) Run(context.Context) {}
func (r *botRoomReader) SamplesMs() []float64 { return nil }
```

- [ ] **Step 4: Build + run preflight test**

Run: `go build ./tools/loadgen/ && go test ./tools/loadgen/ -run TestValidateBotRoomSelection -v`
Expected: clean build + PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/main.go tools/loadgen/botroom.go tools/loadgen/botroom_test.go
git commit -m "feat(loadgen): max-room-size subcommand and per-step ramp driver"
```

---

## Task 8: Optional read driver

**Files:**
- Modify: `tools/loadgen/botroom.go` (replace the `botRoomReader` stub)
- Test: `tools/loadgen/botroom_test.go`

The reader issues the `message.read` RPC (`subject.MessageRead`) against the step's rooms, using the bot account and the most recent message ID the sender published to that room. It times the reply and records latency.

- [ ] **Step 1: Write the failing test**

Add to `tools/loadgen/botroom_test.go`:

```go
func TestBotRoomReader_RecordsLatency(t *testing.T) {
	// Spin a tiny in-process NATS responder using a real connection is heavy;
	// instead unit-test the latency bookkeeping directly.
	r := &botRoomReader{}
	r.record(12 * time.Millisecond)
	r.record(8 * time.Millisecond)
	got := r.SamplesMs()
	require.Len(t, got, 2)
	assert.InDelta(t, 12.0, got[0], 0.001)
}

func TestBotRoomSender_LastMsgIDPerRoom(t *testing.T) {
	m := NewMetrics()
	c := NewCollector(m, "botroom-small")
	pub := &stubBotPublisher{}
	s := NewBotRoomSender(&BotRoomSenderConfig{
		Publisher: pub, Collector: c, Metrics: m, SiteID: "site-local",
		BotAccount: "bot-0", Rooms: []string{"broom-s100-r0"}, Size: 100,
		Rate: 200, MaxInFlight: 50, Content: "x", Preset: "botroom-small",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	assert.NotEmpty(t, s.LastMsgID("broom-s100-r0"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run 'TestBotRoomReader_RecordsLatency|TestBotRoomSender_LastMsgIDPerRoom'`
Expected: FAIL — `r.record undefined`, `s.LastMsgID undefined`.

- [ ] **Step 3: Write minimal implementation**

In `tools/loadgen/botroom.go`, extend `BotRoomSender` to track last msg ID per room. Add a field and update `publishOne`:

```go
// add to BotRoomSender struct:
	lastMsg sync.Map // roomID -> string (most recent published message ID)
```
```go
// in publishOne, immediately after computing msgID:
	s.lastMsg.Store(roomID, msgID)
```
```go
// LastMsgID returns the most recent message ID the sender published to roomID.
func (s *BotRoomSender) LastMsgID(roomID string) string {
	v, ok := s.lastMsg.Load(roomID)
	if !ok {
		return ""
	}
	return v.(string)
}
```

Replace the `botRoomReader` stub with the real implementation:

```go
// botRoomReader issues message.read RPCs against the step's rooms to exercise
// room-service's O(N) read-floor recompute on a large room.
type botRoomReader struct {
	nc      *nats.Conn
	siteID  string
	account string
	rooms   []string
	rate    int
	sender  *BotRoomSender
	cursor  atomic.Uint64

	mu      sync.Mutex
	samples []time.Duration
}

func newBotRoomReader(nc *nats.Conn, siteID, account string, rooms []string, rate int, sender *BotRoomSender) *botRoomReader {
	return &botRoomReader{nc: nc, siteID: siteID, account: account, rooms: rooms, rate: rate, sender: sender}
}

func (r *botRoomReader) record(d time.Duration) {
	r.mu.Lock()
	r.samples = append(r.samples, d)
	r.mu.Unlock()
}

// SamplesMs returns the recorded read latencies in milliseconds.
func (r *botRoomReader) SamplesMs() []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]float64, len(r.samples))
	for i, d := range r.samples {
		out[i] = float64(d.Microseconds()) / 1000.0
	}
	return out
}

func (r *botRoomReader) Run(ctx context.Context) {
	if r.rate <= 0 || len(r.rooms) == 0 {
		return
	}
	interval := time.Second / time.Duration(r.rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.readOne(ctx)
		}
	}
}

func (r *botRoomReader) readOne(ctx context.Context) {
	idx := r.cursor.Add(1) - 1
	roomID := r.rooms[int(idx)%len(r.rooms)]
	lastMsgID := r.sender.LastMsgID(roomID)
	if lastMsgID == "" {
		return // nothing sent to this room yet
	}
	data, err := json.Marshal(map[string]string{"messageId": lastMsgID})
	if err != nil {
		return
	}
	subj := subject.MessageRead(r.account, roomID, r.siteID)
	start := time.Now()
	_, rerr := r.nc.RequestWithContext(ctx, subj, data)
	if rerr != nil {
		if ctx.Err() == nil {
			r.sender.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("timeout").Inc()
		}
		return
	}
	r.record(time.Since(start))
}
```

> `RequestWithContext` enforces the ctx deadline; since the step ctx has no per-request timeout, wrap each call: replace `r.nc.RequestWithContext(ctx, subj, data)` with a 5s-bounded child context:
> ```go
> reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
> _, rerr := r.nc.RequestWithContext(reqCtx, subj, data)
> cancel()
> ```

- [ ] **Step 4: Run tests + build**

Run: `go test ./tools/loadgen/ -run 'TestBotRoomReader|TestBotRoomSender_LastMsgID' -v && go build ./tools/loadgen/`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/botroom.go tools/loadgen/botroom_test.go
git commit -m "feat(loadgen): optional message.read read driver for botroom"
```

---

## Task 9: Companion — lift members add-path ceiling past 1000

**Files:**
- Modify: `tools/loadgen/members.go` (`builtinMembersPresets`)
- Modify: `tools/loadgen/deploy/docker-compose.yml` (room-service `MAX_ROOM_SIZE`)
- Test: `tools/loadgen/members_test.go`

- [ ] **Step 1: Write the failing test**

Add to `tools/loadgen/members_test.go`:

```go
func TestBuiltinMembersPreset_CapacityXL(t *testing.T) {
	p, ok := BuiltinMembersPreset("members-capacity-xl")
	require.True(t, ok)
	// baseline + pool must allow growth to ~5000.
	assert.GreaterOrEqual(t, p.BaselineSize+p.CandidatePool, 5000)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/loadgen/ -run TestBuiltinMembersPreset_CapacityXL`
Expected: FAIL — preset not found.

- [ ] **Step 3: Write minimal implementation**

In `tools/loadgen/members.go`, add to `builtinMembersPresets`:

```go
	"members-capacity-xl": {
		Name: "members-capacity-xl", Users: 12000, Rooms: 3,
		BaselineSize: 1, CandidatePool: 5500, // grows past the old 1000 ceiling
	},
```

In `tools/loadgen/deploy/docker-compose.yml`, change the room-service env line:

```yaml
      - MAX_ROOM_SIZE=6000
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/loadgen/ -run TestBuiltinMembersPreset_CapacityXL -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/members.go tools/loadgen/deploy/docker-compose.yml tools/loadgen/members_test.go
git commit -m "feat(loadgen): members-capacity-xl preset and MAX_ROOM_SIZE=6000 for >1000 add tests"
```

---

## Task 10: Deploy Makefile targets

**Files:**
- Modify: `tools/loadgen/deploy/Makefile`

- [ ] **Step 1: Add targets**

Append to `tools/loadgen/deploy/Makefile` (mirror the existing target style; `$(COMPOSE)` and the `loadgen` service are already defined in the file):

```makefile
seed-botroom:
	@test -n "$(PRESET)" || (echo "PRESET=<botroom-*> required" && exit 1)
	$(COMPOSE) exec -T loadgen /loadgen seed --workload=botroom --preset=$(PRESET)

teardown-botroom:
	@test -n "$(PRESET)" || (echo "PRESET=<botroom-*> required" && exit 1)
	$(COMPOSE) exec -T loadgen /loadgen teardown --workload=botroom --preset=$(PRESET)

run-max-room-size: ## Ramp room size to find the largest a bot can blast (PRESET=.. RATE=.. SIZES=.. ROOMS_PER_SIZE=..)
	@test -n "$(PRESET)" || (echo "PRESET=<botroom-*> required" && exit 1)
	@test -n "$(RATE)" || (echo "RATE=<msgs/sec> required" && exit 1)
	$(COMPOSE) exec -T loadgen /loadgen max-room-size \
	    --preset=$(PRESET) \
	    --rate=$(RATE) \
	    $(if $(SIZES),--sizes=$(SIZES),) \
	    --rooms-per-size=$(or $(ROOMS_PER_SIZE),4) \
	    --reads=$(or $(READS),0) \
	    --hold=$(or $(HOLD),180s) \
	    --csv=/results/botroom-$(PRESET)-$$(date +%Y%m%d-%H%M%S).csv
```

- [ ] **Step 2: Verify Makefile parses**

Run: `make -C tools/loadgen/deploy -n run-max-room-size PRESET=botroom-small RATE=100`
Expected: prints the `docker compose exec` command without error (dry run).

- [ ] **Step 3: Commit**

```bash
git add tools/loadgen/deploy/Makefile
git commit -m "feat(loadgen): deploy targets for botroom seed/teardown/run"
```

---

## Task 11: Integration test

**Files:**
- Create: `tools/loadgen/botroom_integration_test.go`

Uses `pkg/testutil` shared containers per CLAUDE.md. The package's existing integration files (`integration_test.go`, `members_integration_test.go`) **already declare the single `TestMain`** — do NOT add another (duplicate symbol = compile error). This test asserts the botroom fixtures seed correctly into Mongo (the deterministic, harness-light part that is reliable in CI). The optional Step-2 extension drives a real send if the suite's worker harness is reused.

- [ ] **Step 1: Write the test (seed-and-shape assertion)**

```go
//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

func TestBotRoom_SeedShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := testutil.MongoDB(t, "botroom")

	p, _ := BuiltinBotRoomPreset("botroom-small")
	fixtures, layout := BuildBotRoomFixtures(&p, 1, "site-local")
	require.NoError(t, Seed(ctx, db, &fixtures))

	// Each size-50 room must have exactly 50 subscriptions in Mongo.
	count, err := db.Collection("subscriptions").CountDocuments(ctx,
		map[string]any{"roomId": layout.RoomsBySize[50][0]})
	require.NoError(t, err)
	assert.Equal(t, int64(50), count)

	// The bot must be the owner of that room.
	ownerCount, err := db.Collection("subscriptions").CountDocuments(ctx,
		map[string]any{"roomId": layout.RoomsBySize[50][0], "roles": "owner", "u.account": layout.BotAccount})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ownerCount)
}
```

- [ ] **Step 1b (optional): end-to-end send extension**

Only add this if you confirm how the existing loadgen integration suite starts
room-service / broadcast-worker / notification-worker (read `integration_test.go`
first). Connect via `testutil.NATS(t)`, seed room keys with
`SeedRoomKeys`, run a single `runBotRoomStep` at `Rate: 50, Warmup: 5s,
Hold: 10s, Cooldown: 2s` against `layout.RoomsBySize[50]`, and assert
`collector.E2Count() > 0` and zero `gatekeeper` errors via
`gatheredCounterValue(...)`. If the worker harness is not available in this
suite, leave Step 1 as the deliverable and note it in the PR description.

- [ ] **Step 2: Run the integration test**

Run: `make test-integration SERVICE=loadgen`
Expected: PASS (Docker required).

- [ ] **Step 3: Commit**

```bash
git add tools/loadgen/botroom_integration_test.go
git commit -m "test(loadgen): botroom integration test (seed + send + verdict)"
```

---

## Task 12: README + final verification

**Files:**
- Modify: `tools/loadgen/README.md`

- [ ] **Step 1: Add a "Large-room bot scenario (max-room-size)" section**

Document, mirroring the existing scenario sections: quick start (`seed-botroom`, `run-max-room-size`), the presets table (`botroom-small`, `botroom-medium`), the flags (`--rate`, `--sizes`, `--rooms-per-size`, `--reads`, SLO flags), how to read the ANSWER line, the single-vs-multiple-room note (`--rooms-per-size=1` probes the Cassandra hot-partition / Mongo hot-doc limit; default 4 measures aggregate fan-out + cache churn), the notification-worker-as-headline-signal note, and the companion `members-capacity-xl` + `MAX_ROOM_SIZE=6000` note for add-path-past-1000. Add a "v2 follow-ups" note: create-and-blast pattern and a live N-connection NATS-delivery pool.

```markdown
## Large-room bot scenario (max-room-size)

Finds the largest room a bot can blast at a fixed send rate before an SLO
signal breaks — gating on **notification-worker** consumer backlog as the
headline O(N)-per-message signal (notification-worker is NOT exempt here,
unlike the daily scenario).

### Quick start

```
make -C tools/loadgen/deploy up
make -C tools/loadgen/deploy seed-botroom PRESET=botroom-medium
make -C tools/loadgen/deploy run-max-room-size PRESET=botroom-medium RATE=200
```

### Presets

| preset          | sizes                      | rooms/size | users | use case          |
|-----------------|----------------------------|------------|-------|-------------------|
| `botroom-small` | 50, 100, 200               | 4          | 300   | smoke / dev       |
| `botroom-medium`| 100, 500, 1000, 2000, 5000 | 4          | 5500  | default capacity  |

### One room vs many

`--rooms-per-size=1` concentrates the rate on a single room — probes the
Cassandra hot-partition (`messages_by_room` key `(room_id, bucket)`) and the
Mongo room-doc write contention (`UpdateRoomLastMessage`). The default `4`
spreads the rate to measure aggregate fan-out plus member-list cache churn.

### v2 follow-ups (not yet built)

- Create-and-blast: bots create a ~100-member room and immediately send
  (cold-cache penalty).
- Live N-connection pool to measure NATS core delivery fan-out to real member
  connections.
```

- [ ] **Step 2: Full verification**

Run:
```bash
make generate SERVICE=loadgen   # no store interfaces changed, but confirm clean
make fmt
make lint
make test SERVICE=loadgen
```
Expected: lint clean, all unit tests pass with `-race`.

- [ ] **Step 3: Commit**

```bash
git add tools/loadgen/README.md
git commit -m "docs(loadgen): large-room bot scenario README"
```

---

## Self-Review Notes (for the executor)

- **Spec coverage:** Pattern 1 (bot blasting large rooms, ramp size, `--rate` flag) → Tasks 1,3,4,5,6,7. notification-worker headline signal → Task 5. `--rooms-per-size` default 4 → Tasks 1,7,12. Read-side concern → Task 8 (`--reads`, off by default). Add-path past 1000 → Task 9. Seed-direct-to-Mongo bypassing `MAX_ROOM_SIZE`, bot-as-owner gatekeeper bypass → Task 1. Deferred v2 items documented → Task 12.
- **Cross-task type consistency:** `BotRoomSenderConfig` has a `Preset string` field (Task 4) consumed in Task 7's `runBotRoomStep` (`Preset: c.Preset`). `botRoomReader` is stubbed in Task 7 and replaced in Task 8. `atomicInt64` is defined in Task 7 (`main.go`) and used by both the e1 handler and `runBotRoomStep`. `BotRoomSender.LastMsgID`/`lastMsg` are added in Task 8 and consumed by `botRoomReader`.
- **Reused-as-is APIs** are listed in the File Structure section with exact signatures — do not redefine them.
</content>
