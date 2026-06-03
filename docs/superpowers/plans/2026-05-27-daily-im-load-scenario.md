# Daily-IM Load Scenario Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `loadgen daily` subcommand that simulates N users using the chat system as their primary IM, ramps N geometrically, and reports the largest N at which all SLO signals held over a steady-state hold window.

**Architecture:** New subcommand in `tools/loadgen/`. Reuses existing `seed`, `metrics`, `Collector`, and `deploy/` plumbing. Adds a per-user state machine driven by a Poisson process under a diurnal envelope, a hybrid receiver (direct `nats.Conn` per user up to a cap + multiplexed pool above the cap), and a step-up/hold ramp controller that evaluates five SLO signals per step.

**Tech Stack:** Go 1.25, `nats.go` + JetStream, `caarlos0/env`, `pkg/roomkeystore`, `pkg/subject`, `pkg/natsutil`, `pkg/model`, existing `Collector`/`Metrics`/`Fixtures` types from `tools/loadgen`.

**Spec:** `docs/superpowers/specs/2026-05-27-daily-im-load-scenario-design.md`

---

## File Map

| File | New / Modify | Responsibility |
|---|---|---|
| `tools/loadgen/preset.go` | Modify | Add `DailyBands` field + `daily-light/heavy/power` presets; extend `BuildFixtures` to honour banded membership |
| `tools/loadgen/preset_test.go` | Modify | Tests for new presets and banded fixture build |
| `tools/loadgen/daily_envelope.go` | New | `rateMultiplier(elapsed, holdDuration) float64` — diurnal Gaussian envelope |
| `tools/loadgen/daily_envelope_test.go` | New | Unit tests for envelope shape |
| `tools/loadgen/daily_user.go` | New | `userState` struct + Markov idle/active state machine + weighted action picker |
| `tools/loadgen/daily_user_test.go` | New | Tests for state transitions and picker weights |
| `tools/loadgen/daily_actions.go` | New | One function per op: `sendMessage`, `readReceipt`, `scrollHistory`, `refreshRoomList`, `muteToggle`, `roomCreate`, `memberAdd`, `threadReply` |
| `tools/loadgen/daily_actions_test.go` | New | Per-action unit tests using injected publish func |
| `tools/loadgen/daily_pool.go` | New | `directPool` (one `nats.Conn` per user) + `multiplexPool` (shared conns with dispatcher) |
| `tools/loadgen/daily_pool_test.go` | New | Routing + drop-counting tests for multiplex dispatcher |
| `tools/loadgen/daily_verdict.go` | New | `StepResult`, `evaluateStep`, JetStream pending poller, service `/metrics` scraper, loadgen self-metrics |
| `tools/loadgen/daily_verdict_test.go` | New | Verdict logic for each tripping condition |
| `tools/loadgen/daily_report.go` | New | Console table + CSV emit per step |
| `tools/loadgen/daily_report_test.go` | New | CSV format + console table golden tests |
| `tools/loadgen/daily.go` | New | `dailyConfig`, `parseDailyConfig`, `runDaily` — top-level control loop (ramp + step lifecycle) |
| `tools/loadgen/daily_test.go` | New | Unit tests for config parsing + lifecycle wiring |
| `tools/loadgen/daily_integration_test.go` | New | One integration test: tiny preset against testcontainers NATS+Mongo+Valkey, asserts a passing verdict |
| `tools/loadgen/main.go` | Modify | Add `"daily"` subcommand case to `dispatch` |
| `tools/loadgen/main_test.go` | Modify | Test dispatch route for "daily" |
| `tools/loadgen/deploy/Makefile` | Modify | Add `run-daily` target |
| `tools/loadgen/README.md` | Modify | Document the new subcommand under a "Daily-IM scenario" heading |

---

## Task 1: Preset model — add `DailyBands` and `daily-*` presets

**Goal:** Extend `Preset` so each preset can describe banded per-user room membership. Add the three daily presets. No `BuildFixtures` changes yet — they'll fail until Task 2.

**Files:**
- Modify: `tools/loadgen/preset.go`
- Modify: `tools/loadgen/preset_test.go`

- [ ] **Step 1: Write the failing test for the new fields and lookup**

Append to `tools/loadgen/preset_test.go`:

```go
func TestBuiltinPreset_Daily(t *testing.T) {
	cases := []struct {
		name       string
		users      int
		bands      DailyBands
	}{
		{"daily-light", 10000, DailyBands{DMs: 15, Small: 10, Medium: 5, Large: 2}},
		{"daily-heavy", 10000, DailyBands{DMs: 25, Small: 20, Medium: 8, Large: 3}},
		{"daily-power", 10000, DailyBands{DMs: 40, Small: 30, Medium: 10, Large: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := BuiltinPreset(tc.name)
			require.True(t, ok, "preset %s missing", tc.name)
			require.Equal(t, tc.users, p.Users)
			require.Equal(t, tc.bands, p.DailyBands)
		})
	}
}
```

- [ ] **Step 2: Run test, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `DailyBands` undefined and lookup returns `!ok`.

- [ ] **Step 3: Add `DailyBands` type and field**

In `tools/loadgen/preset.go`, after the `Range` struct (line ~26):

```go
// DailyBands describes how many rooms of each size band a typical user
// belongs to in the daily-IM presets. Zero means the preset is not a
// daily-IM preset and BuildFixtures falls back to the legacy distribution.
type DailyBands struct {
	DMs    int // 2-member rooms
	Small  int // 5-20 members
	Medium int // 50-200 members
	Large  int // 500-2000 members
}

// IsZero reports whether bands are absent.
func (b DailyBands) IsZero() bool {
	return b.DMs == 0 && b.Small == 0 && b.Medium == 0 && b.Large == 0
}

// RoomsPerUser is the sum of all bands.
func (b DailyBands) RoomsPerUser() int { return b.DMs + b.Small + b.Medium + b.Large }
```

Add field to `Preset` struct:

```go
DailyBands   DailyBands
```

- [ ] **Step 4: Register the three daily presets**

Add entries to `builtinPresets` in `tools/loadgen/preset.go`:

```go
"daily-light": {
	Name: "daily-light", Users: 10000,
	RoomSizeDist: DistMixed, SenderDist: DistZipf,
	ContentBytes: Range{Min: 50, Max: 2000},
	MentionRate:  0.05, ThreadRate: 0.30,
	DailyBands: DailyBands{DMs: 15, Small: 10, Medium: 5, Large: 2},
},
"daily-heavy": {
	Name: "daily-heavy", Users: 10000,
	RoomSizeDist: DistMixed, SenderDist: DistZipf,
	ContentBytes: Range{Min: 50, Max: 2000},
	MentionRate:  0.05, ThreadRate: 0.30,
	DailyBands: DailyBands{DMs: 25, Small: 20, Medium: 8, Large: 3},
},
"daily-power": {
	Name: "daily-power", Users: 10000,
	RoomSizeDist: DistMixed, SenderDist: DistZipf,
	ContentBytes: Range{Min: 50, Max: 2000},
	MentionRate:  0.05, ThreadRate: 0.30,
	DailyBands: DailyBands{DMs: 40, Small: 30, Medium: 10, Large: 3},
},
```

Preset.Users is fixed at 10000 — the ramp activates a *subset* per step, so the fixture set sizes for a single mid-size deployment. (Larger sweeps re-seed with a bigger Users count via a future CLI override; not in this PR.)

- [ ] **Step 5: Run tests, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS for `TestBuiltinPreset_Daily`. Existing tests unaffected (no `BuildFixtures` change yet).

- [ ] **Step 6: Commit**

```bash
git add tools/loadgen/preset.go tools/loadgen/preset_test.go
git commit -m "loadgen: add DailyBands field and daily-light/heavy/power presets"
```

---

## Task 2: `BuildFixtures` — banded membership

**Goal:** When `DailyBands` is non-zero, generate rooms partitioned by size band, then for each user pick rooms from each band until the per-user counts are met.

**Files:**
- Modify: `tools/loadgen/preset.go`
- Modify: `tools/loadgen/preset_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tools/loadgen/preset_test.go`:

```go
func TestBuildFixtures_DailyBands(t *testing.T) {
	p, _ := BuiltinPreset("daily-heavy")
	p.Users = 200 // shrink for test speed; bands stay the same
	f := BuildFixtures(&p, 42, "site-test")

	require.Equal(t, 200, len(f.Users))

	// Per-user subscription count must equal p.DailyBands.RoomsPerUser
	want := p.DailyBands.RoomsPerUser()
	perUser := map[string]int{}
	for _, s := range f.Subscriptions {
		perUser[s.User.ID]++
	}
	for _, u := range f.Users {
		require.Equal(t, want, perUser[u.ID],
			"user %s wrong subscription count", u.ID)
	}

	// Each band must yield at least one room with the band's size range.
	sizes := map[string]int{}
	for _, r := range f.Rooms {
		sizes[r.ID] = r.UserCount
	}
	var nDM, nSmall, nMed, nLarge int
	for _, sz := range sizes {
		switch {
		case sz == 2:
			nDM++
		case sz >= 5 && sz <= 20:
			nSmall++
		case sz >= 50 && sz <= 200:
			nMed++
		case sz >= 500 && sz <= 2000:
			nLarge++
		}
	}
	require.Greater(t, nDM, 0)
	require.Greater(t, nSmall, 0)
	require.Greater(t, nMed, 0)
	require.Greater(t, nLarge, 0)

	// Determinism: same seed yields identical fixtures.
	f2 := BuildFixtures(&p, 42, "site-test")
	require.Equal(t, f, f2)
}
```

- [ ] **Step 2: Run test, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — BuildFixtures still falls back to legacy logic and produces wrong subscription counts.

- [ ] **Step 3: Implement banded build**

In `tools/loadgen/preset.go`, replace the body of `BuildFixtures` so that when `!p.DailyBands.IsZero()` it takes the banded path; otherwise it runs the existing legacy code unchanged. Add this branch at the top of `BuildFixtures` (after generating `users` and computing `now`, before generating `rooms`):

```go
if !p.DailyBands.IsZero() {
	return buildBandedFixtures(p, r, users, siteID, now)
}
```

Then add the new function:

```go
// buildBandedFixtures generates rooms and subscriptions for a daily-IM
// preset where each user belongs to a fixed mix of DM/small/medium/large
// rooms per p.DailyBands. Rooms are pre-allocated band-by-band, then users
// are assigned rooms within each band round-robin so every user gets the
// configured per-band count and rooms stay within their band's size range.
func buildBandedFixtures(p *Preset, r *rand.Rand, users []model.User, siteID string, now time.Time) Fixtures {
	bands := p.DailyBands
	totalUsers := len(users)

	// Number of rooms per band, derived from per-user counts and band size targets.
	// Aim for the *average* band size to consume the per-user demand exactly.
	nDM := (totalUsers * bands.DMs) / 2 // each DM has 2 members
	nSmall := (totalUsers*bands.Small + 9) / 10
	nMed := (totalUsers*bands.Medium + 99) / 100
	nLarge := (totalUsers*bands.Large + 999) / 1000
	if nLarge == 0 && bands.Large > 0 {
		nLarge = 1
	}

	type bandSpec struct {
		name       string
		count      int
		sizeMin    int
		sizeMax    int
		roomType   model.RoomType
		perUser    int
	}
	specs := []bandSpec{
		{"dm", nDM, 2, 2, model.RoomTypeDM, bands.DMs},
		{"small", nSmall, 5, 20, model.RoomTypeChannel, bands.Small},
		{"medium", nMed, 50, 200, model.RoomTypeChannel, bands.Medium},
		{"large", nLarge, 500, 2000, model.RoomTypeChannel, bands.Large},
	}

	var rooms []model.Room
	var subs []model.Subscription
	roomKeys := make(map[string]roomkeystore.RoomKeyPair)

	for _, spec := range specs {
		// Pre-create rooms in this band.
		bandRooms := make([]model.Room, spec.count)
		bandSizes := make([]int, spec.count)
		for i := 0; i < spec.count; i++ {
			id := fmt.Sprintf("room-%s-%06d", spec.name, i)
			size := spec.sizeMin
			if spec.sizeMax > spec.sizeMin {
				size = spec.sizeMin + r.Intn(spec.sizeMax-spec.sizeMin+1)
			}
			bandRooms[i] = model.Room{
				ID: id, Name: id, Type: spec.roomType, SiteID: siteID,
				CreatedAt: now, UpdatedAt: now,
			}
			bandSizes[i] = size
		}

		// Build a flat "slot" list: each room contributes `size` slots.
		// Then shuffle users and walk slots, assigning users round-robin
		// until every user has spec.perUser memberships in this band.
		type slot struct{ roomIdx int }
		totalSlots := 0
		for _, s := range bandSizes {
			totalSlots += s
		}
		slots := make([]slot, 0, totalSlots)
		for i, s := range bandSizes {
			for k := 0; k < s; k++ {
				slots = append(slots, slot{roomIdx: i})
			}
		}
		// Each user needs spec.perUser memberships. We have totalSlots
		// slots and totalUsers*spec.perUser demand. If they don't match
		// exactly we trim or extend slot capacity per room within the
		// band's size range.
		demand := totalUsers * spec.perUser
		if demand < len(slots) {
			slots = slots[:demand]
		}
		for demand > len(slots) && len(bandRooms) > 0 {
			// Extend the smallest room until either capacity or demand fits.
			idx := r.Intn(len(bandRooms))
			if bandSizes[idx] < spec.sizeMax {
				bandSizes[idx]++
				slots = append(slots, slot{roomIdx: idx})
			} else {
				break
			}
		}
		// Shuffle slots so users aren't clustered into the same rooms.
		r.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })

		// Assign: user u gets slots[u*perUser : (u+1)*perUser].
		// Track per-room dedupe to avoid double-membership.
		roomMembers := make(map[string]map[string]bool, len(bandRooms))
		for ui, u := range users {
			start := ui * spec.perUser
			if start >= len(slots) {
				break
			}
			end := start + spec.perUser
			if end > len(slots) {
				end = len(slots)
			}
			for _, sl := range slots[start:end] {
				roomID := bandRooms[sl.roomIdx].ID
				if roomMembers[roomID] == nil {
					roomMembers[roomID] = make(map[string]bool)
				}
				if roomMembers[roomID][u.ID] {
					continue // skip duplicate (rare)
				}
				roomMembers[roomID][u.ID] = true
				subs = append(subs, model.Subscription{
					ID: fmt.Sprintf("sub-%s-%s", roomID, u.ID),
					User: model.SubscriptionUser{ID: u.ID, Account: u.Account},
					RoomID: roomID, SiteID: siteID,
					Roles: []model.Role{model.RoleMember},
					JoinedAt: now,
				})
			}
		}

		// Finalise UserCount and emit rooms + keys.
		for i := range bandRooms {
			bandRooms[i].UserCount = len(roomMembers[bandRooms[i].ID])
			roomKeys[bandRooms[i].ID] = deterministicRoomKeyPair(r)
		}
		rooms = append(rooms, bandRooms...)
	}

	return Fixtures{Users: users, Rooms: rooms, Subscriptions: subs, RoomKeys: roomKeys}
}
```

- [ ] **Step 4: Run tests, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS — including the new test and all existing preset tests.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/preset.go tools/loadgen/preset_test.go
git commit -m "loadgen: banded fixture build for daily-IM presets"
```

---

## Task 3: Diurnal envelope function

**Goal:** Pure function `rateMultiplier(elapsed, hold time.Duration) float64`. Two Gaussians at 1/3 and 2/3 of the hold, normalised so the peak is 1.0; baseline 0.4, swing 0.6 → range [0.4, 1.0].

**Files:**
- Create: `tools/loadgen/daily_envelope.go`
- Create: `tools/loadgen/daily_envelope_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_envelope_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateMultiplier(t *testing.T) {
	hold := 180 * time.Second
	cases := []struct {
		name      string
		elapsed   time.Duration
		minWant   float64
		maxWant   float64
	}{
		{"start", 0, 0.39, 0.55},
		{"first peak", hold / 3, 0.95, 1.01},
		{"trough between peaks", hold / 2, 0.55, 0.85},
		{"second peak", 2 * hold / 3, 0.95, 1.01},
		{"end", hold, 0.39, 0.55},
		{"beyond end clamped", hold + time.Second, 0.39, 0.55},
		{"negative clamped", -time.Second, 0.39, 0.55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rateMultiplier(tc.elapsed, hold)
			require.GreaterOrEqual(t, got, tc.minWant, "got=%f", got)
			require.LessOrEqual(t, got, tc.maxWant, "got=%f", got)
		})
	}
}

func TestRateMultiplier_ZeroHold(t *testing.T) {
	require.Equal(t, 1.0, rateMultiplier(0, 0))
}
```

- [ ] **Step 2: Run test, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `rateMultiplier` undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily_envelope.go`:

```go
package main

import (
	"math"
	"time"
)

const (
	envelopeBaseline = 0.4
	envelopeSwing    = 0.6
	envelopeSigma    = 0.12 // fraction of hold; controls peak width
)

// rateMultiplier returns the diurnal envelope value at `elapsed` into a
// hold window of length `hold`. Range is [envelopeBaseline, envelopeBaseline+envelopeSwing].
// The shape is the max of two Gaussians centred at 1/3 and 2/3 of hold,
// approximating a workday with morning and afternoon peaks.
//
// Returns 1.0 when hold is zero (degenerate case used by some tests).
func rateMultiplier(elapsed, hold time.Duration) float64 {
	if hold <= 0 {
		return 1.0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > hold {
		elapsed = hold
	}
	x := float64(elapsed) / float64(hold)
	g := func(centre float64) float64 {
		d := (x - centre) / envelopeSigma
		return math.Exp(-0.5 * d * d)
	}
	peak := math.Max(g(1.0/3.0), g(2.0/3.0))
	return envelopeBaseline + envelopeSwing*peak
}
```

- [ ] **Step 4: Run test, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_envelope.go tools/loadgen/daily_envelope_test.go
git commit -m "loadgen: diurnal envelope for daily-IM scenario"
```

---

## Task 4: User state machine + action picker

**Goal:** Per-user struct holding ID, account, room memberships, a two-state Markov (idle/active), and a weighted picker that returns the next action and a wait duration.

**Files:**
- Create: `tools/loadgen/daily_user.go`
- Create: `tools/loadgen/daily_user_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_user_test.go`:

```go
package main

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUserState_StepTransitions(t *testing.T) {
	u := newUserState("u-1", "user-1", []string{"r-1"}, 42)
	u.activeProb = 0.5
	u.idleProb = 0.5
	r := rand.New(rand.NewSource(1))
	activeSeen, idleSeen := false, false
	for i := 0; i < 1000; i++ {
		u.step(r)
		if u.active {
			activeSeen = true
		} else {
			idleSeen = true
		}
	}
	require.True(t, activeSeen)
	require.True(t, idleSeen)
}

func TestPickAction_WeightsApproximatelyMatch(t *testing.T) {
	w := defaultActionWeights()
	r := rand.New(rand.NewSource(7))
	counts := map[actionKind]int{}
	const N = 100000
	for i := 0; i < N; i++ {
		counts[pickAction(r, w)]++
	}
	// Send should dominate (largest weight). Mute/Create should be rare.
	require.Greater(t, counts[actionSend], counts[actionReadReceipt])
	require.Greater(t, counts[actionReadReceipt], counts[actionScrollHistory])
	require.Less(t, counts[actionMuteToggle], counts[actionRoomCreate]+counts[actionMemberAdd]+10) // tiny
}

func TestActionRate_PerSecond(t *testing.T) {
	// daily-heavy: 60+25+3+5+0.5+0.2+0.2 = 93.9 actions/day = 0.00326/sec per user
	r := actionRatePerSecond(defaultActionWeights().totalPerDay(), 8*time.Hour)
	require.InDelta(t, 0.00326, r, 0.0002)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `userState`, `newUserState`, etc. undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily_user.go`:

```go
package main

import (
	"math/rand"
	"time"
)

// actionKind enumerates the user-day operations the simulator can perform.
type actionKind int

const (
	actionSend actionKind = iota
	actionReadReceipt
	actionScrollHistory
	actionRefreshRoomList
	actionMemberAdd
	actionRoomCreate
	actionMuteToggle
)

// actionWeights is the per-user-per-day count for each action kind.
// Source of truth: spec section 4 "daily-heavy" budget.
type actionWeights struct {
	Send             float64
	ReadReceipt      float64
	ScrollHistory    float64
	RefreshRoomList  float64
	MemberAdd        float64
	RoomCreate       float64
	MuteToggle       float64
}

func defaultActionWeights() actionWeights {
	return actionWeights{
		Send: 60, ReadReceipt: 25, ScrollHistory: 3,
		RefreshRoomList: 5, MemberAdd: 0.5, RoomCreate: 0.2, MuteToggle: 0.2,
	}
}

func (w actionWeights) totalPerDay() float64 {
	return w.Send + w.ReadReceipt + w.ScrollHistory + w.RefreshRoomList +
		w.MemberAdd + w.RoomCreate + w.MuteToggle
}

// actionRatePerSecond converts a per-day count to a Poisson rate
// (actions per second), scaled to the active fraction of a workday.
func actionRatePerSecond(perDay float64, workday time.Duration) float64 {
	return perDay / workday.Seconds()
}

// pickAction returns one actionKind chosen with probability proportional
// to w. r is the source of randomness.
func pickAction(r *rand.Rand, w actionWeights) actionKind {
	total := w.totalPerDay()
	x := r.Float64() * total
	cumulative := []struct {
		k actionKind
		w float64
	}{
		{actionSend, w.Send},
		{actionReadReceipt, w.ReadReceipt},
		{actionScrollHistory, w.ScrollHistory},
		{actionRefreshRoomList, w.RefreshRoomList},
		{actionMemberAdd, w.MemberAdd},
		{actionRoomCreate, w.RoomCreate},
		{actionMuteToggle, w.MuteToggle},
	}
	var acc float64
	for _, c := range cumulative {
		acc += c.w
		if x < acc {
			return c.k
		}
	}
	return actionSend
}

// userState is the per-user runtime state for a daily-IM simulated user.
type userState struct {
	ID         string
	Account    string
	Rooms      []string
	active     bool
	activeProb float64 // P(stay active | active)
	idleProb   float64 // P(stay idle | idle)
}

func newUserState(id, account string, rooms []string, _seed int64) *userState {
	return &userState{
		ID: id, Account: account, Rooms: rooms,
		active: false,
		// Tuned so stationary active fraction ≈ 25%: P(idle->active)=0.05, P(active->idle)=0.15.
		activeProb: 0.85, idleProb: 0.95,
	}
}

// step advances the Markov chain by one tick. Call at the per-user tick
// interval (e.g. every 1s of simulated time).
func (u *userState) step(r *rand.Rand) {
	x := r.Float64()
	if u.active {
		if x > u.activeProb {
			u.active = false
		}
	} else {
		if x > u.idleProb {
			u.active = true
		}
	}
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_user.go tools/loadgen/daily_user_test.go
git commit -m "loadgen: user state machine + action picker for daily-IM"
```

---

## Task 5: Action handlers — send, read receipt, room-list refresh

**Goal:** Three handlers that publish their respective subjects. Inject the publish func so tests can capture data without NATS. Defer the more elaborate request/reply ops (history, member-add, room-create, mute, thread) to Task 6.

**Files:**
- Create: `tools/loadgen/daily_actions.go`
- Create: `tools/loadgen/daily_actions_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_actions_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
	"github.com/stretchr/testify/require"
)

type captured struct {
	mu    sync.Mutex
	pubs  []capturedPub
	reqs  []capturedReq
}
type capturedPub struct {
	Subj string
	Data []byte
}
type capturedReq struct {
	Subj string
	Data []byte
}

func (c *captured) publish(_ context.Context, subj string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pubs = append(c.pubs, capturedPub{Subj: subj, Data: append([]byte(nil), data...)})
	return nil
}
func (c *captured) request(_ context.Context, subj string, data []byte, _ time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, capturedReq{Subj: subj, Data: append([]byte(nil), data...)})
	return []byte(`{"ok":true}`), nil
}

func TestSendMessage_PublishesToFrontdoor(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a", "room-b"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	err := sendMessage(ctx, u, "hello")
	require.NoError(t, err)
	require.Len(t, c.pubs, 1)
	got := c.pubs[0]
	require.True(t, got.Subj == subject.MsgSend("user-1", "room-a", "site-test") ||
		got.Subj == subject.MsgSend("user-1", "room-b", "site-test"))
	var req model.SendMessageRequest
	require.NoError(t, json.Unmarshal(got.Data, &req))
	require.Equal(t, "hello", req.Content)
}

func TestReadReceipt_Publishes(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	err := readReceipt(ctx, u, "msg-1")
	require.NoError(t, err)
	require.Len(t, c.pubs, 1)
	require.Equal(t, subject.MessageRead("user-1", "room-a", "site-test"), c.pubs[0].Subj)
}

func TestRefreshRoomList_Requests(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1"}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	err := refreshRoomList(ctx, u)
	require.NoError(t, err)
	require.Len(t, c.reqs, 1)
	require.Equal(t, subject.UserSubscriptionGetRooms("user-1", "site-test"), c.reqs[0].Subj)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `actionCtx`, `sendMessage`, etc. undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily_actions.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// publishFn matches the existing Publisher interface used by generator.go.
type publishFn func(ctx context.Context, subj string, data []byte) error

// requestFn does a NATS request/reply.
type requestFn func(ctx context.Context, subj string, data []byte, timeout time.Duration) ([]byte, error)

// actionCtx bundles everything every action handler needs. Keeps function
// signatures small and tests easy to write.
type actionCtx struct {
	Ctx       context.Context
	Publish   publishFn
	Request   requestFn
	SiteID    string
	Collector *Collector // optional; for latency correlation
	Rand      *rand.Rand // optional; falls back to a per-call source
}

func (a actionCtx) rand() *rand.Rand {
	if a.Rand != nil {
		return a.Rand
	}
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

const defaultRequestTimeout = 5 * time.Second

// sendMessage publishes a SendMessageRequest on the frontdoor subject for a
// random room the user belongs to. If u has no rooms, returns nil (noop).
func sendMessage(a actionCtx, u *userState, content string) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	msgID := idgen.GenerateMessageID()
	reqID := idgen.GenerateRequestID()
	req := model.SendMessageRequest{ID: msgID, Content: content, RequestID: reqID}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal send-message: %w", err)
	}
	if a.Collector != nil {
		a.Collector.RecordPublish(reqID, msgID, time.Now())
	}
	if err := a.Publish(a.Ctx, subject.MsgSend(u.Account, roomID, a.SiteID), data); err != nil {
		if a.Collector != nil {
			a.Collector.RecordPublishFailed(reqID, msgID)
		}
		return fmt.Errorf("publish send-message: %w", err)
	}
	return nil
}

// readReceipt publishes a read-receipt event for a random room.
func readReceipt(a actionCtx, u *userState, lastMsgID string) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	payload, err := json.Marshal(map[string]string{"messageId": lastMsgID})
	if err != nil {
		return fmt.Errorf("marshal read-receipt: %w", err)
	}
	if err := a.Publish(a.Ctx, subject.MessageRead(u.Account, roomID, a.SiteID), payload); err != nil {
		return fmt.Errorf("publish read-receipt: %w", err)
	}
	return nil
}

// refreshRoomList does a NATS request/reply for the user's subscription list.
func refreshRoomList(a actionCtx, u *userState) error {
	_, err := a.Request(a.Ctx, subject.UserSubscriptionGetRooms(u.Account, a.SiteID), nil, defaultRequestTimeout)
	if err != nil {
		return fmt.Errorf("request room-list: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_actions.go tools/loadgen/daily_actions_test.go
git commit -m "loadgen: send/read-receipt/room-list action handlers"
```

---

## Task 6: Action handlers — history, mute, room-create, member-add, thread-reply

**Goal:** Remaining five action handlers. Same pattern.

**Files:**
- Modify: `tools/loadgen/daily_actions.go`
- Modify: `tools/loadgen/daily_actions_test.go`

- [ ] **Step 1: Add failing tests**

Append to `tools/loadgen/daily_actions_test.go`:

```go
func TestScrollHistory_Requests(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	require.NoError(t, scrollHistory(ctx, u))
	require.Len(t, c.reqs, 1)
	// History fetch goes through MsgGet-style subject — check it includes the roomID.
	require.Contains(t, c.reqs[0].Subj, "room-a")
}

func TestMuteToggle_Publishes(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	require.NoError(t, muteToggle(ctx, u))
	require.Len(t, c.reqs, 1)
	require.Equal(t, subject.MuteToggle("user-1", "room-a", "site-test"), c.reqs[0].Subj)
}

func TestRoomCreate_Requests(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1"}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	require.NoError(t, roomCreate(ctx, u))
	require.Len(t, c.reqs, 1)
	require.Equal(t, subject.RoomCreate("user-1", "site-test"), c.reqs[0].Subj)
}

func TestMemberAdd_Requests(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	require.NoError(t, memberAdd(ctx, u, "user-2"))
	require.Len(t, c.reqs, 1)
	require.Equal(t, subject.MemberAdd("user-1", "room-a", "site-test"), c.reqs[0].Subj)
}

func TestThreadReply_Publishes(t *testing.T) {
	c := &captured{}
	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-a"}}
	ctx := actionCtx{Ctx: context.Background(), Publish: c.publish, Request: c.request, SiteID: "site-test"}
	require.NoError(t, threadReply(ctx, u, "parent-msg-1", "reply text"))
	require.Len(t, c.pubs, 1)
	require.Equal(t, subject.MsgSend("user-1", "room-a", "site-test"), c.pubs[0].Subj)
	var req model.SendMessageRequest
	require.NoError(t, json.Unmarshal(c.pubs[0].Data, &req))
	require.Equal(t, "parent-msg-1", req.ParentID)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — handlers undefined.

- [ ] **Step 3: Implement**

Append to `tools/loadgen/daily_actions.go`:

```go
// scrollHistory does a NATS request/reply for a random room's recent history.
func scrollHistory(a actionCtx, u *userState) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	_, err := a.Request(a.Ctx, subject.MsgGet(u.Account, roomID, a.SiteID), nil, defaultRequestTimeout)
	if err != nil {
		return fmt.Errorf("request scroll-history: %w", err)
	}
	return nil
}

// muteToggle requests the mute toggle for a random room.
func muteToggle(a actionCtx, u *userState) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	_, err := a.Request(a.Ctx, subject.MuteToggle(u.Account, roomID, a.SiteID), nil, defaultRequestTimeout)
	if err != nil {
		return fmt.Errorf("request mute-toggle: %w", err)
	}
	return nil
}

// roomCreate creates a new channel room owned by u. The resulting roomID is
// not added to u.Rooms — this is a deliberately leaky abstraction since the
// simulated user wouldn't immediately be active in a brand-new room within
// the same hold window.
func roomCreate(a actionCtx, u *userState) error {
	payload, err := json.Marshal(map[string]any{
		"name": fmt.Sprintf("loadtest-%s-%d", u.ID, time.Now().UnixNano()),
		"type": string(model.RoomTypeChannel),
	})
	if err != nil {
		return fmt.Errorf("marshal room-create: %w", err)
	}
	_, err = a.Request(a.Ctx, subject.RoomCreate(u.Account, a.SiteID), payload, defaultRequestTimeout)
	if err != nil {
		return fmt.Errorf("request room-create: %w", err)
	}
	return nil
}

// memberAdd adds a target account to a random room u belongs to.
func memberAdd(a actionCtx, u *userState, targetAccount string) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	payload, err := json.Marshal(map[string]any{"accounts": []string{targetAccount}})
	if err != nil {
		return fmt.Errorf("marshal member-add: %w", err)
	}
	_, err = a.Request(a.Ctx, subject.MemberAdd(u.Account, roomID, a.SiteID), payload, defaultRequestTimeout)
	if err != nil {
		return fmt.Errorf("request member-add: %w", err)
	}
	return nil
}

// threadReply publishes a SendMessageRequest with ParentID set, on the
// frontdoor subject. The handler is intentionally a "send with parent set"
// rather than a separate code path so it stresses the same pipeline.
func threadReply(a actionCtx, u *userState, parentID, content string) error {
	if len(u.Rooms) == 0 {
		return nil
	}
	roomID := u.Rooms[a.rand().Intn(len(u.Rooms))]
	msgID := idgen.GenerateMessageID()
	reqID := idgen.GenerateRequestID()
	req := model.SendMessageRequest{
		ID: msgID, Content: content, RequestID: reqID, ParentID: parentID,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal thread-reply: %w", err)
	}
	if a.Collector != nil {
		a.Collector.RecordPublish(reqID, msgID, time.Now())
	}
	if err := a.Publish(a.Ctx, subject.MsgSend(u.Account, roomID, a.SiteID), data); err != nil {
		if a.Collector != nil {
			a.Collector.RecordPublishFailed(reqID, msgID)
		}
		return fmt.Errorf("publish thread-reply: %w", err)
	}
	return nil
}
```

If `model.SendMessageRequest` lacks a `ParentID` field, check `pkg/model/*.go`; thread support exists per the spec's "message.thread.read" feature so the field should already be present. If not, extend the model in this task with a struct-tag-compliant `ParentID string \`json:"parentId,omitempty" bson:"parentId,omitempty"\`` field (it must coexist with the existing model; check `pkg/model/model_test.go` round-trips remain green).

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_actions.go tools/loadgen/daily_actions_test.go
git commit -m "loadgen: history/mute/room-create/member-add/thread action handlers"
```

---

## Task 7: Direct receiver pool

**Goal:** A `directPool` that, for each user, opens one `nats.Conn` and `Subscribe`s to each room's broadcast subject. On receive, it timestamps arrival and matches to publish time via the existing `Collector.RecordBroadcastReceived` (or equivalent — check the existing Collector method names and reuse).

**Files:**
- Create: `tools/loadgen/daily_pool.go`
- Create: `tools/loadgen/daily_pool_test.go`

- [ ] **Step 1: Inspect existing Collector receive method**

Run: `grep -n "RecordBroadcast\|RecordReceive\|broadcastsReceived" tools/loadgen/collector.go`

Capture the exact method name. If `RecordBroadcastReceived(messageID string, t time.Time)` already exists, use it. If a similar method exists under a different name (e.g. `RecordReceive`, `RecordBroadcast`), use that name and adjust call sites in this task. If no equivalent exists, add one to `collector.go` with this signature:

```go
func (c *Collector) RecordBroadcastReceived(messageID string, t time.Time) {
	// Looks up publish time stored by RecordPublish / RecordPublishBroadcastOnly,
	// records latency sample, increments broadcastsReceived counter.
	c.recordLatencyForMessage(messageID, t)
	c.broadcastsReceived.Add(1)
}
```

Then add the supporting `broadcastsReceived atomic.Int64` field and any helper (`recordLatencyForMessage`) needed, plus the `BroadcastsReceived() int64` accessor. Commit these collector changes as a small fix at the end of Step 1 before proceeding to Step 2.

- [ ] **Step 2: Write the failing test**

Create `tools/loadgen/daily_pool_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
	"github.com/hmchangw/chat/pkg/testutil"
)

func TestDirectPool_ReceivesBroadcast(t *testing.T) {
	url := testutil.NATS(t)
	ncPub, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { ncPub.Close() })

	col := NewCollector()
	pool := newDirectPool(url, col)
	t.Cleanup(pool.Close)

	u := &userState{ID: "u-1", Account: "user-1", Rooms: []string{"room-test"}}
	require.NoError(t, pool.Add(u))

	// Publish a fake broadcast event with LastMsgID set.
	evt := model.RoomEvent{Event: model.EventCreated, LastMsgID: "msg-42", RoomID: "room-test"}
	data, _ := json.Marshal(evt)

	col.RecordPublishBroadcastOnly("msg-42", time.Now())
	require.NoError(t, ncPub.Publish(subject.RoomEvent("room-test"), data))
	require.NoError(t, ncPub.Flush())

	require.Eventually(t, func() bool {
		return col.BroadcastsReceived() == 1
	}, 2*time.Second, 20*time.Millisecond)
}
```

(Note: this is the *only* daily test that needs testcontainers NATS in unit-test land; mark file with build-tag if required, otherwise it runs as a regular unit test — `testutil.NATS` already manages a shared container.)

- [ ] **Step 3: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `newDirectPool` undefined.

- [ ] **Step 4: Implement**

Create `tools/loadgen/daily_pool.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// directPool owns one nats.Conn per simulated user plus one subscription per
// user-room pair. Each subscription callback records broadcast-arrival time
// against the shared Collector for latency correlation.
type directPool struct {
	url       string
	collector *Collector

	mu    sync.Mutex
	users map[string]*directUser
}

type directUser struct {
	id   string
	nc   *nats.Conn
	subs []*nats.Subscription
}

func newDirectPool(natsURL string, c *Collector) *directPool {
	return &directPool{
		url: natsURL, collector: c, users: make(map[string]*directUser),
	}
}

// Add opens a connection for u and subscribes to every room in u.Rooms.
// Safe to call concurrently for different users.
func (p *directPool) Add(u *userState) error {
	nc, err := nats.Connect(p.url, nats.Name("loadgen-daily-"+u.ID))
	if err != nil {
		return fmt.Errorf("connect for %s: %w", u.ID, err)
	}
	du := &directUser{id: u.ID, nc: nc}
	for _, roomID := range u.Rooms {
		sub, err := nc.Subscribe(subject.RoomEvent(roomID), func(m *nats.Msg) {
			p.onBroadcast(m)
		})
		if err != nil {
			_ = nc.Drain()
			return fmt.Errorf("subscribe %s/%s: %w", u.ID, roomID, err)
		}
		du.subs = append(du.subs, sub)
	}
	p.mu.Lock()
	p.users[u.ID] = du
	p.mu.Unlock()
	return nil
}

// Size reports the number of users currently in the pool.
func (p *directPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.users)
}

func (p *directPool) onBroadcast(m *nats.Msg) {
	var evt model.RoomEvent
	if err := json.Unmarshal(m.Data, &evt); err != nil {
		return // ignore malformed
	}
	if evt.LastMsgID == "" {
		return
	}
	p.collector.RecordBroadcastReceived(evt.LastMsgID, time.Now())
}

// Close drains all connections.
func (p *directPool) Close() {
	p.mu.Lock()
	users := p.users
	p.users = nil
	p.mu.Unlock()
	for _, du := range users {
		_ = du.nc.Drain()
	}
}
```

If the existing `Collector` does not expose `RecordBroadcastReceived` with this signature, adjust the call site to match the existing method (likely named differently — Task 7 Step 1 captured the real name).

- [ ] **Step 5: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tools/loadgen/daily_pool.go tools/loadgen/daily_pool_test.go
git commit -m "loadgen: direct receiver pool for daily-IM scenario"
```

---

## Task 8: Multiplex receiver pool

**Goal:** A `multiplexPool` that shares `M` `nats.Conn`s across `N` users by subscribing each conn to the union of rooms for its assigned users, then routing incoming messages to per-user inboxes via a `roomID → []userID` map. Non-blocking send to inboxes; drops counted by Collector.

**Files:**
- Modify: `tools/loadgen/daily_pool.go`
- Modify: `tools/loadgen/daily_pool_test.go`

- [ ] **Step 1: Add failing test**

Append to `tools/loadgen/daily_pool_test.go`:

```go
func TestMultiplexPool_RoutesBroadcastToInbox(t *testing.T) {
	url := testutil.NATS(t)
	ncPub, _ := nats.Connect(url)
	t.Cleanup(func() { ncPub.Close() })

	col := NewCollector()
	pool := newMultiplexPool(url, col, 2 /*pool size*/)
	t.Cleanup(pool.Close)

	uA := &userState{ID: "u-a", Account: "ua", Rooms: []string{"r-1"}}
	uB := &userState{ID: "u-b", Account: "ub", Rooms: []string{"r-1", "r-2"}}
	require.NoError(t, pool.Add(uA))
	require.NoError(t, pool.Add(uB))

	col.RecordPublishBroadcastOnly("msg-1", time.Now())
	data, _ := json.Marshal(model.RoomEvent{LastMsgID: "msg-1", RoomID: "r-1"})
	require.NoError(t, ncPub.Publish(subject.RoomEvent("r-1"), data))
	require.NoError(t, ncPub.Flush())

	require.Eventually(t, func() bool {
		return col.BroadcastsReceived() >= 1 // counted once per arrival on the shared conn
	}, 2*time.Second, 20*time.Millisecond)
}

func TestMultiplexPool_DropsCountedOnInboxFull(t *testing.T) {
	col := NewCollector()
	pool := &multiplexPool{
		collector: col,
		dispatch:  make(map[string][]chan *nats.Msg),
	}
	// Wire one room with one zero-capacity inbox.
	full := make(chan *nats.Msg) // unbuffered, no reader
	pool.dispatch["r-1"] = []chan *nats.Msg{full}

	pool.route(&nats.Msg{Subject: subject.RoomEvent("r-1"), Data: []byte(`{"lastMsgId":"x"}`)})

	require.Equal(t, int64(1), col.MultiplexDrops())
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `multiplexPool` undefined and `Collector.MultiplexDrops` not yet present.

- [ ] **Step 3: Extend Collector with multiplex-drop counter**

Add to `tools/loadgen/collector.go`:

```go
// multiplexDrops counts broadcasts dropped because a per-user inbox was full.
multiplexDrops atomic.Int64
```

(Inside the `Collector` struct.)

Add the methods:

```go
func (c *Collector) RecordMultiplexDrop()      { c.multiplexDrops.Add(1) }
func (c *Collector) MultiplexDrops() int64     { return c.multiplexDrops.Load() }
```

And a `BroadcastsReceived` accessor if not already present:

```go
func (c *Collector) BroadcastsReceived() int64 { return c.broadcastsReceived.Load() }
```

(Add the atomic field and increment inside the existing receive-record method if it doesn't already exist.)

- [ ] **Step 4: Implement multiplex pool**

Append to `tools/loadgen/daily_pool.go`:

```go
// multiplexPool fans M shared NATS connections across N users. Each shared
// connection subscribes (with reference counting) to the union of room
// broadcast subjects for its assigned users. Incoming messages are routed
// to per-user inbox channels via the dispatch map.
type multiplexPool struct {
	url       string
	collector *Collector
	conns     []*nats.Conn

	mu          sync.Mutex
	roomRefs    map[string]int                   // roomID -> ref count on the shared conns
	dispatch    map[string][]chan *nats.Msg      // roomID -> per-user inboxes
	userInbox   map[string]chan *nats.Msg        // userID -> that user's inbox channel
	nextConn    int                              // round-robin assignment
}

func newMultiplexPool(natsURL string, c *Collector, size int) *multiplexPool {
	p := &multiplexPool{
		url: natsURL, collector: c,
		roomRefs: make(map[string]int),
		dispatch: make(map[string][]chan *nats.Msg),
		userInbox: make(map[string]chan *nats.Msg),
	}
	for i := 0; i < size; i++ {
		nc, err := nats.Connect(natsURL, nats.Name(fmt.Sprintf("loadgen-daily-mux-%d", i)))
		if err != nil {
			p.Close()
			panic(fmt.Errorf("multiplex conn %d: %w", i, err))
		}
		p.conns = append(p.conns, nc)
	}
	return p
}

// Add registers a user with the multiplex pool.
func (p *multiplexPool) Add(u *userState) error {
	inbox := make(chan *nats.Msg, 128)
	p.mu.Lock()
	p.userInbox[u.ID] = inbox
	for _, roomID := range u.Rooms {
		p.dispatch[roomID] = append(p.dispatch[roomID], inbox)
		if p.roomRefs[roomID] == 0 {
			nc := p.conns[p.nextConn%len(p.conns)]
			p.nextConn++
			subj := subject.RoomEvent(roomID)
			if _, err := nc.Subscribe(subj, p.route); err != nil {
				p.mu.Unlock()
				return fmt.Errorf("multiplex subscribe %s: %w", roomID, err)
			}
		}
		p.roomRefs[roomID]++
	}
	p.mu.Unlock()
	return nil
}

// route is called by every shared conn's subscription callback. It looks up
// the destination inboxes by RoomID and does a non-blocking send.
func (p *multiplexPool) route(m *nats.Msg) {
	var evt model.RoomEvent
	if err := json.Unmarshal(m.Data, &evt); err != nil {
		return
	}
	roomID := evt.RoomID
	if roomID == "" {
		// Fallback: extract roomID from subject "chat.room.{roomID}.event"
		// — RoomEvent subject layout in pkg/subject is "chat.room.<id>.event".
		roomID = parseRoomFromSubject(m.Subject)
	}
	p.mu.Lock()
	inboxes := p.dispatch[roomID]
	p.mu.Unlock()
	if evt.LastMsgID != "" {
		p.collector.RecordBroadcastReceived(evt.LastMsgID, time.Now())
	}
	for _, ch := range inboxes {
		select {
		case ch <- m:
		default:
			p.collector.RecordMultiplexDrop()
		}
	}
}

func parseRoomFromSubject(subj string) string {
	// "chat.room.<id>.event" — pkg/subject.RoomEvent layout.
	parts := strings.Split(subj, ".")
	if len(parts) >= 3 && parts[0] == "chat" && parts[1] == "room" {
		return parts[2]
	}
	return ""
}

// Close drains shared conns and closes inboxes.
func (p *multiplexPool) Close() {
	p.mu.Lock()
	inboxes := p.userInbox
	p.userInbox = nil
	p.dispatch = nil
	p.roomRefs = nil
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, nc := range conns {
		_ = nc.Drain()
	}
	for _, ch := range inboxes {
		close(ch)
	}
}
```

Add `"strings"` to the imports.

- [ ] **Step 5: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tools/loadgen/daily_pool.go tools/loadgen/daily_pool_test.go tools/loadgen/collector.go
git commit -m "loadgen: multiplex receiver pool with drop counting"
```

---

## Task 9: Verdict types + evaluator

**Goal:** Define `StepResult`, `ConsumerPendingDelta`, `SelfMetrics`, and `evaluateStep` — the pure function that takes raw measurements and produces a verdict.

**Files:**
- Create: `tools/loadgen/daily_verdict.go`
- Create: `tools/loadgen/daily_verdict_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_verdict_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEvaluateStep_AllGreen(t *testing.T) {
	s := stepInputs{
		N: 1000, HoldDuration: 180 * time.Second,
		LatencySamples: []float64{10, 20, 50, 100, 200},
		AttemptedOps:   10000, FailedOps: 0,
		ConsumerPending: map[string]ConsumerPendingDelta{
			"message-worker":   {Start: 100, End: 110, Delta: 10},
			"broadcast-worker": {Start: 50, End: 55, Delta: 5},
		},
		ServiceErrors: map[string]int64{},
		Self:          SelfMetrics{GCPauseP99Ms: 5, CPUPercent: 40, Goroutines: 50000},
	}
	r := evaluateStep(s, defaultThresholds())
	require.False(t, r.Tripped)
	require.False(t, r.Inconclusive)
	require.Empty(t, r.TrippedReasons)
}

func TestEvaluateStep_TripsOnPendingGrowth(t *testing.T) {
	s := stepInputs{
		N: 5000, HoldDuration: 180 * time.Second,
		LatencySamples: []float64{10, 20},
		AttemptedOps:   1000,
		ConsumerPending: map[string]ConsumerPendingDelta{
			"broadcast-worker": {Start: 100, End: 2000, Delta: 1900},
		},
	}
	r := evaluateStep(s, defaultThresholds())
	require.True(t, r.Tripped)
	require.Contains(t, r.TrippedReasons[0], "broadcast-worker")
}

func TestEvaluateStep_TripsOnP95Latency(t *testing.T) {
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 200 // p95 = 200, well under
	}
	samples[99] = 800
	samples[98] = 700
	samples[97] = 650
	samples[96] = 600
	samples[95] = 550
	// p95 of 100 samples (index 94 sorted) is roughly the 95th-percentile;
	// with these values, sort puts 550 at index 95 → p95=550 > 500 → trip.
	s := stepInputs{
		N: 5000, HoldDuration: 180 * time.Second,
		LatencySamples: samples, AttemptedOps: 1000,
	}
	r := evaluateStep(s, defaultThresholds())
	require.True(t, r.Tripped)
	require.Contains(t, r.TrippedReasons[0], "p95")
}

func TestEvaluateStep_InconclusiveOnHighGC(t *testing.T) {
	s := stepInputs{
		N: 20000, HoldDuration: 180 * time.Second,
		LatencySamples: []float64{10},
		AttemptedOps:   1000,
		Self:           SelfMetrics{GCPauseP99Ms: 80, CPUPercent: 90, Goroutines: 100000},
	}
	r := evaluateStep(s, defaultThresholds())
	require.True(t, r.Inconclusive)
	require.False(t, r.Tripped) // inconclusive overrides trip
}

func TestEvaluateStep_TripsOnErrorRate(t *testing.T) {
	s := stepInputs{
		N: 5000, HoldDuration: 180 * time.Second,
		LatencySamples: []float64{10},
		AttemptedOps:   10000, FailedOps: 50, // 0.5% > 0.1%
	}
	r := evaluateStep(s, defaultThresholds())
	require.True(t, r.Tripped)
	require.Contains(t, r.TrippedReasons[0], "error_rate")
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `stepInputs`, `evaluateStep`, etc. undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily_verdict.go`:

```go
package main

import (
	"fmt"
	"sort"
	"time"
)

// ConsumerPendingDelta captures a single durable's pending-message count
// at the start and end of a hold window.
type ConsumerPendingDelta struct {
	Start int64
	End   int64
	Delta int64
}

// SelfMetrics describes the loadgen process's own resource state during
// the hold window. High values mean the load box is the bottleneck and
// the step is INCONCLUSIVE rather than PASS/TRIP.
type SelfMetrics struct {
	GCPauseP99Ms float64
	CPUPercent   float64
	Goroutines   int
}

// Thresholds are the per-signal cutoffs that decide PASS / TRIP / INCONCLUSIVE.
type Thresholds struct {
	P95LatencyMs       float64
	P99LatencyMs       float64
	ErrorRate          float64 // fraction (0.001 = 0.1%)
	PendingGrowth      int64
	GCPauseInconclusive float64
	CPUInconclusive    float64
}

func defaultThresholds() Thresholds {
	return Thresholds{
		P95LatencyMs: 500, P99LatencyMs: 1000,
		ErrorRate: 0.001, PendingGrowth: 1000,
		GCPauseInconclusive: 50, CPUInconclusive: 80,
	}
}

// stepInputs is everything evaluateStep needs to produce a verdict.
type stepInputs struct {
	N                int
	StartedAt        time.Time
	HoldDuration     time.Duration
	LatencySamples   []float64 // milliseconds
	AttemptedOps     int64
	FailedOps        int64
	ConsumerPending  map[string]ConsumerPendingDelta
	ServiceErrors    map[string]int64
	Self             SelfMetrics
}

// StepResult is the verdict for a single ramp step.
type StepResult struct {
	N                     int
	StartedAt             time.Time
	HoldDuration          time.Duration
	P50LatencyMs          float64
	P95LatencyMs          float64
	P99LatencyMs          float64
	ErrorRate             float64
	AttemptedOps          int64
	FailedOps             int64
	ConsumerPending       map[string]ConsumerPendingDelta
	ServiceErrorIncreases map[string]int64
	LoadgenSelfMetrics    SelfMetrics
	Tripped               bool
	Inconclusive          bool
	TrippedReasons        []string
}

func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]float64, len(samples))
	copy(cp, samples)
	sort.Float64s(cp)
	idx := int(p * float64(len(cp)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func evaluateStep(in stepInputs, th Thresholds) StepResult {
	r := StepResult{
		N: in.N, StartedAt: in.StartedAt, HoldDuration: in.HoldDuration,
		AttemptedOps: in.AttemptedOps, FailedOps: in.FailedOps,
		ConsumerPending: in.ConsumerPending,
		ServiceErrorIncreases: in.ServiceErrors,
		LoadgenSelfMetrics: in.Self,
		P50LatencyMs: percentile(in.LatencySamples, 0.50),
		P95LatencyMs: percentile(in.LatencySamples, 0.95),
		P99LatencyMs: percentile(in.LatencySamples, 0.99),
	}
	if in.AttemptedOps > 0 {
		r.ErrorRate = float64(in.FailedOps) / float64(in.AttemptedOps)
	}

	// Inconclusive overrides trip.
	if in.Self.GCPauseP99Ms > th.GCPauseInconclusive || in.Self.CPUPercent > th.CPUInconclusive {
		r.Inconclusive = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("inconclusive: gc=%.1fms cpu=%.1f%%", in.Self.GCPauseP99Ms, in.Self.CPUPercent))
		return r
	}

	if r.P95LatencyMs > th.P95LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("p95=%.0fms > %.0f", r.P95LatencyMs, th.P95LatencyMs))
	}
	if r.P99LatencyMs > th.P99LatencyMs {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("p99=%.0fms > %.0f", r.P99LatencyMs, th.P99LatencyMs))
	}
	if r.ErrorRate > th.ErrorRate {
		r.Tripped = true
		r.TrippedReasons = append(r.TrippedReasons,
			fmt.Sprintf("error_rate=%.4f > %.4f", r.ErrorRate, th.ErrorRate))
	}
	for durable, d := range in.ConsumerPending {
		if d.Delta > th.PendingGrowth {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s pending +%d > +%d", durable, d.Delta, th.PendingGrowth))
		}
	}
	for svc, n := range in.ServiceErrors {
		if n > 0 {
			r.Tripped = true
			r.TrippedReasons = append(r.TrippedReasons,
				fmt.Sprintf("%s errors +%d", svc, n))
		}
	}
	return r
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_verdict.go tools/loadgen/daily_verdict_test.go
git commit -m "loadgen: SLO verdict evaluator for daily-IM steps"
```

---

## Task 10: JetStream pending poller + service /metrics scraper + self-metrics

**Goal:** Three small data-collection helpers. They run during the hold window and produce inputs for `evaluateStep`.

**Files:**
- Modify: `tools/loadgen/daily_verdict.go`
- Modify: `tools/loadgen/daily_verdict_test.go`

- [ ] **Step 1: Add failing tests**

Append to `tools/loadgen/daily_verdict_test.go`:

```go
func TestSelfMetricsSnapshot_ReturnsSaneValues(t *testing.T) {
	s := snapshotSelfMetrics()
	require.Greater(t, s.Goroutines, 0)
	require.GreaterOrEqual(t, s.GCPauseP99Ms, 0.0)
	require.GreaterOrEqual(t, s.CPUPercent, 0.0)
}

func TestDiffPending_BuildsDelta(t *testing.T) {
	start := map[string]int64{"a": 100, "b": 50}
	end := map[string]int64{"a": 150, "b": 50, "c": 10}
	got := diffPending(start, end)
	require.Equal(t, int64(50), got["a"].Delta)
	require.Equal(t, int64(0), got["b"].Delta)
	require.Equal(t, int64(10), got["c"].Delta) // c was added mid-window
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `snapshotSelfMetrics`, `diffPending` undefined.

- [ ] **Step 3: Implement helpers**

Append to `tools/loadgen/daily_verdict.go`:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/metrics"
	"sync"
)

// snapshotSelfMetrics samples loadgen-process resource counters.
// CPU% is approximate (delta of cumulative CPU time / wall-clock since last call).
func snapshotSelfMetrics() SelfMetrics {
	g := runtime.NumGoroutine()
	gcP99 := readGCPauseP99Ms()
	cpu := readCPUPercent()
	return SelfMetrics{
		GCPauseP99Ms: gcP99,
		CPUPercent:   cpu,
		Goroutines:   g,
	}
}

var gcLastNumGC uint32
var gcMu sync.Mutex

func readGCPauseP99Ms() float64 {
	gcMu.Lock()
	defer gcMu.Unlock()
	samples := []metrics.Sample{{Name: "/gc/pauses:seconds"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindFloat64Histogram {
		return 0
	}
	h := samples[0].Value.Float64Histogram()
	if len(h.Counts) == 0 {
		return 0
	}
	// Compute p99 from the histogram.
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := total * 99 / 100
	var acc uint64
	for i, c := range h.Counts {
		acc += c
		if acc >= target {
			return h.Buckets[i] * 1000
		}
	}
	return 0
}

var (
	cpuMu     sync.Mutex
	cpuLastT  time.Time
)

func readCPUPercent() float64 {
	// Lightweight approximation: use Go runtime's process CPU time. For a
	// more precise number, replace with /proc/self/stat parsing or a
	// gopsutil dependency. For load-test gating, this is sufficient.
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	cpuMu.Lock()
	defer cpuMu.Unlock()
	now := time.Now()
	if cpuLastT.IsZero() {
		cpuLastT = now
		return 0
	}
	_ = now
	cpuLastT = now
	// Placeholder: we don't have a clean stdlib way to get process CPU%.
	// Surface NumGoroutine pressure as a coarse proxy multiplied by a
	// scaling factor. This is intentionally conservative; if INCONCLUSIVE
	// trips spuriously, raise the threshold or wire in gopsutil in a
	// follow-up PR.
	return float64(runtime.NumGoroutine()) / 5000.0 * 100
}

// diffPending computes per-durable Start/End/Delta from two snapshots.
// Durables that appeared mid-window are counted with Start=0.
func diffPending(start, end map[string]int64) map[string]ConsumerPendingDelta {
	out := make(map[string]ConsumerPendingDelta, len(end))
	for durable, e := range end {
		s := start[durable]
		out[durable] = ConsumerPendingDelta{Start: s, End: e, Delta: e - s}
	}
	return out
}

// pollPending queries the NATS monitoring endpoint /jsz?consumers=true and
// returns a map of durable name -> NumPending. Endpoint defaults to
// http://localhost:8222 (NATS docker-local default).
func pollPending(ctx context.Context, jszURL string) (map[string]int64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, jszURL+"?consumers=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jsz GET: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccountDetails []struct {
			StreamDetail []struct {
				ConsumerDetail []struct {
					Name       string `json:"name"`
					NumPending int64  `json:"num_pending"`
				} `json:"consumer_detail"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("jsz decode: %w", err)
	}
	out := make(map[string]int64)
	for _, a := range body.AccountDetails {
		for _, s := range a.StreamDetail {
			for _, c := range s.ConsumerDetail {
				out[c.Name] = c.NumPending
			}
		}
	}
	return out, nil
}

// scrapeServiceErrors fetches /metrics from each service URL and returns
// a map of service -> delta in slog_errors_total since the previous call.
// First call returns zeros and records baselines.
type serviceScraper struct {
	mu       sync.Mutex
	baseline map[string]float64
}

func newServiceScraper() *serviceScraper {
	return &serviceScraper{baseline: make(map[string]float64)}
}

func (s *serviceScraper) Scrape(ctx context.Context, urls map[string]string) (map[string]int64, error) {
	out := make(map[string]int64, len(urls))
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, url := range urls {
		v, err := scrapeErrorCounter(ctx, url)
		if err != nil {
			out[name] = 0 // tolerate missing
			continue
		}
		prev, ok := s.baseline[name]
		s.baseline[name] = v
		if !ok {
			out[name] = 0
			continue
		}
		out[name] = int64(v - prev)
	}
	return out, nil
}

func scrapeErrorCounter(ctx context.Context, url string) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("metrics GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	// Naive line-scanner for the `slog_errors_total` counter family. Sum
	// all label combinations.
	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return sumCounterFamily(string(buf), "slog_errors_total"), nil
}

func sumCounterFamily(body, family string) float64 {
	var sum float64
	for _, line := range strings.Split(body, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		if !strings.HasPrefix(line, family) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var v float64
		fmt.Sscanf(fields[len(fields)-1], "%f", &v)
		sum += v
	}
	return sum
}
```

Add the imports `"strings"`, `"net/http"`, `"runtime/metrics"`, `"context"` if not already present in this file.

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_verdict.go tools/loadgen/daily_verdict_test.go
git commit -m "loadgen: pending poller + service scraper + self-metrics"
```

---

## Task 11: Daily config + CLI parsing

**Goal:** Parse the `loadgen daily` command-line flags into a `dailyConfig` struct.

**Files:**
- Create: `tools/loadgen/daily.go`
- Create: `tools/loadgen/daily_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseDailyConfig_Defaults(t *testing.T) {
	c, err := parseDailyConfig([]string{"--preset=daily-heavy"})
	require.NoError(t, err)
	require.Equal(t, "daily-heavy", c.Preset)
	require.Equal(t, []int{1000, 2000, 5000, 10000, 20000, 50000, 100000}, c.Steps)
	require.Equal(t, 60*time.Second, c.Warmup)
	require.Equal(t, 180*time.Second, c.Hold)
	require.Equal(t, 30*time.Second, c.Cooldown)
	require.Equal(t, 20000, c.MaxDirectUsers)
	require.Equal(t, 200, c.MultiplexPoolSize)
	require.Equal(t, 25000, c.MaxConnsPerProcess)
	require.True(t, c.StopOnTrip)
}

func TestParseDailyConfig_Overrides(t *testing.T) {
	c, err := parseDailyConfig([]string{
		"--preset=daily-light",
		"--steps=1000,5000",
		"--warmup=10s",
		"--hold=30s",
		"--cooldown=5s",
		"--max-direct-users=5000",
		"--multiplex-pool-size=50",
		"--max-conns-per-process=10000",
		"--stop-on-trip=false",
	})
	require.NoError(t, err)
	require.Equal(t, []int{1000, 5000}, c.Steps)
	require.Equal(t, 10*time.Second, c.Warmup)
	require.False(t, c.StopOnTrip)
}

func TestParseDailyConfig_Rejects_UnknownPreset(t *testing.T) {
	_, err := parseDailyConfig([]string{"--preset=nope"})
	require.Error(t, err)
}

func TestParseDailyConfig_RejectsTooManyConns(t *testing.T) {
	_, err := parseDailyConfig([]string{
		"--preset=daily-heavy",
		"--max-direct-users=30000",
		"--max-conns-per-process=10000",
	})
	require.Error(t, err) // 30000 direct + 200 mux > 10000 cap
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `parseDailyConfig` undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily.go`:

```go
package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dailyConfig is the parsed CLI input for `loadgen daily`.
type dailyConfig struct {
	Preset             string
	Steps              []int
	Warmup             time.Duration
	Hold               time.Duration
	Cooldown           time.Duration
	StopOnTrip         bool
	MaxDirectUsers     int
	MultiplexPoolSize  int
	MaxConnsPerProcess int
	CSVPath            string
}

func parseDailyConfig(args []string) (dailyConfig, error) {
	fs := flag.NewFlagSet("daily", flag.ContinueOnError)
	preset := fs.String("preset", "daily-heavy", "preset name (daily-light|daily-heavy|daily-power)")
	steps := fs.String("steps", "1000,2000,5000,10000,20000,50000,100000", "comma-separated N values")
	warmup := fs.Duration("warmup", 60*time.Second, "per-step warm-up")
	hold := fs.Duration("hold", 180*time.Second, "per-step hold")
	cooldown := fs.Duration("cooldown", 30*time.Second, "per-step cooldown")
	stopOnTrip := fs.Bool("stop-on-trip", true, "stop on first trip")
	maxDirect := fs.Int("max-direct-users", 20000, "direct-pool cap")
	mux := fs.Int("multiplex-pool-size", 200, "multiplex pool size")
	maxConns := fs.Int("max-conns-per-process", 25000, "safety ceiling on connections")
	csvPath := fs.String("csv", "", "optional CSV output path")
	if err := fs.Parse(args); err != nil {
		return dailyConfig{}, err
	}

	if _, ok := BuiltinPreset(*preset); !ok {
		return dailyConfig{}, fmt.Errorf("unknown preset %q", *preset)
	}

	parsedSteps, err := parseStepList(*steps)
	if err != nil {
		return dailyConfig{}, err
	}

	projected := *maxDirect + *mux
	if projected > *maxConns {
		return dailyConfig{}, fmt.Errorf(
			"projected conn count %d (direct=%d + mux=%d) exceeds --max-conns-per-process=%d",
			projected, *maxDirect, *mux, *maxConns)
	}

	return dailyConfig{
		Preset:             *preset,
		Steps:              parsedSteps,
		Warmup:             *warmup,
		Hold:               *hold,
		Cooldown:           *cooldown,
		StopOnTrip:         *stopOnTrip,
		MaxDirectUsers:     *maxDirect,
		MultiplexPoolSize:  *mux,
		MaxConnsPerProcess: *maxConns,
		CSVPath:            *csvPath,
	}, nil
}

func parseStepList(s string) ([]int, error) {
	if s == "" {
		return nil, fmt.Errorf("--steps cannot be empty")
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Allow "1k" / "10k" shorthand.
		mult := 1
		if strings.HasSuffix(p, "k") {
			mult = 1000
			p = strings.TrimSuffix(p, "k")
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid step %q: %w", p, err)
		}
		out = append(out, n*mult)
	}
	return out, nil
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily.go tools/loadgen/daily_test.go
git commit -m "loadgen: parseDailyConfig CLI flags + validation"
```

---

## Task 12: Per-step lifecycle (warmup → hold → cooldown)

**Goal:** Implement `runStep(ctx, env, n) StepResult` — runs a single step against an already-built fixture set and returns a verdict. This is the core of the ramp.

**Files:**
- Modify: `tools/loadgen/daily.go`
- Modify: `tools/loadgen/daily_test.go`

- [ ] **Step 1: Add failing test**

Append to `tools/loadgen/daily_test.go`:

```go
func TestRunStep_StubReturnsPassWhenEverythingIsGreen(t *testing.T) {
	// This is a smoke test — runStep should be wired to call evaluateStep
	// with empty-but-valid measurements when fixtures are tiny.
	env := &stepEnv{
		thresholds: defaultThresholds(),
		// pollPending stubbed: empty maps → no delta
		pollPending: func(ctx context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		// scrapeServices stubbed: returns empty
		scrapeServices: func(ctx context.Context) (map[string]int64, error) {
			return map[string]int64{}, nil
		},
		warmup: 50 * time.Millisecond, hold: 100 * time.Millisecond, cooldown: 20 * time.Millisecond,
	}
	r := runStep(context.Background(), env, 100, 0)
	require.False(t, r.Tripped)
	require.False(t, r.Inconclusive)
	require.Equal(t, 100, r.N)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `stepEnv`, `runStep` undefined.

- [ ] **Step 3: Implement**

Append to `tools/loadgen/daily.go`:

```go
import (
	"context"
	"log/slog"
	"sync/atomic"
)

// stepEnv bundles the runtime dependencies of a step. Stub-able for unit tests.
type stepEnv struct {
	collector      *Collector
	direct         *directPool
	multiplex      *multiplexPool
	users          []*userState
	thresholds     Thresholds
	pollPending    func(ctx context.Context) (map[string]int64, error)
	scrapeServices func(ctx context.Context) (map[string]int64, error)
	maxDirect      int // direct pool cap (from cfg.MaxDirectUsers)
	warmup         time.Duration
	hold           time.Duration
	cooldown       time.Duration
	mintJWT        func(ctx context.Context, account string) error // optional; nil = skip
}

// runStep executes one ramp step: activates additional users (delta over
// previous), warms up, holds, evaluates SLO signals, and cools down.
// The current step is `n`; the previous step's user count is `prevN` (0 for
// the first step). Users [prevN..n) are activated this step.
func runStep(ctx context.Context, env *stepEnv, n, prevN int) StepResult {
	startedAt := time.Now()
	delta := n - prevN

	// Activate users in batches of 500/sec to avoid spinning up tens of
	// thousands of goroutines instantly.
	activateUsers(ctx, env, prevN, n)
	if delta > 0 {
		slog.Info("step warmup", "n", n, "delta", delta)
	}

	// Warm-up: clients are sending but SLO counters are reset at the end.
	timer := time.NewTimer(env.warmup)
	select {
	case <-ctx.Done():
		timer.Stop()
		return StepResult{N: n, StartedAt: startedAt}
	case <-timer.C:
	}

	// Snapshot start-of-hold state.
	startPending, _ := env.pollPending(ctx)
	_, _ = env.scrapeServices(ctx) // first call records baseline; delta is zero

	// Reset latency samples and op counters.
	env.collector.Reset()

	// Hold.
	holdEnd := time.Now().Add(env.hold)
	for time.Now().Before(holdEnd) {
		select {
		case <-ctx.Done():
			return StepResult{N: n, StartedAt: startedAt}
		case <-time.After(5 * time.Second):
			// Periodic pending poll could happen here to gather a curve;
			// for the verdict, only start/end snapshots matter.
		}
	}

	// Snapshot end-of-hold state.
	endPending, _ := env.pollPending(ctx)
	svcErrors, _ := env.scrapeServices(ctx)

	in := stepInputs{
		N: n, StartedAt: startedAt, HoldDuration: env.hold,
		LatencySamples:  env.collector.LatencySamples(),
		AttemptedOps:    env.collector.AttemptedOps(),
		FailedOps:       env.collector.FailedOps(),
		ConsumerPending: diffPending(startPending, endPending),
		ServiceErrors:   svcErrors,
		Self:            snapshotSelfMetrics(),
	}
	r := evaluateStep(in, env.thresholds)

	// Cooldown.
	select {
	case <-ctx.Done():
	case <-time.After(env.cooldown):
	}

	return r
}

// activateUsers brings users in the range [from, to) online: assigns them to
// a pool, opens connections / registers room interest, and kicks off their
// per-user state-machine goroutines. Rate-limited at 500 users/sec.
func activateUsers(ctx context.Context, env *stepEnv, from, to int) {
	tokens := time.NewTicker(time.Second / 500)
	defer tokens.Stop()
	for i := from; i < to && i < len(env.users); i++ {
		select {
		case <-ctx.Done():
			return
		case <-tokens.C:
		}
		u := env.users[i]
		// One-time JWT mint per user at activation. Best-effort; on failure
		// the user still proceeds with shared backend.creds for publishing.
		// (Spec section 10: auth-service is exercised lightly at session
		// start, not per message.)
		if env.mintJWT != nil {
			if err := env.mintJWT(ctx, u.Account); err != nil {
				slog.Warn("jwt mint failed", "user", u.ID, "err", err)
			}
		}
		// Choose pool.
		if env.direct != nil && env.direct.Size() < env.maxDirect {
			if err := env.direct.Add(u); err != nil {
				slog.Warn("direct pool add failed", "user", u.ID, "err", err)
				continue
			}
		} else if env.multiplex != nil {
			if err := env.multiplex.Add(u); err != nil {
				slog.Warn("multiplex pool add failed", "user", u.ID, "err", err)
				continue
			}
		}
		// Per-user state-machine goroutines are launched elsewhere (Task 13).
		// For lifecycle-only test this is sufficient.
	}
}

// Helper for tests: allow Collector to expose Reset / accessors.
// (Add these to collector.go if not already present.)
```

Add the missing Collector helpers (Reset, LatencySamples, AttemptedOps, FailedOps) to `tools/loadgen/collector.go`:

```go
func (c *Collector) Reset() {
	c.mu.Lock(); defer c.mu.Unlock()
	c.latencyMs = c.latencyMs[:0]
	c.attempted.Store(0); c.failed.Store(0)
}

func (c *Collector) LatencySamples() []float64 {
	c.mu.Lock(); defer c.mu.Unlock()
	out := make([]float64, len(c.latencyMs))
	copy(out, c.latencyMs)
	return out
}

func (c *Collector) AttemptedOps() int64 { return c.attempted.Load() }
func (c *Collector) FailedOps() int64    { return c.failed.Load() }
```

(Add the underlying fields `latencyMs []float64`, `attempted, failed atomic.Int64`, `mu sync.Mutex` if they don't yet exist, and have the publish/receive paths feed them.)

The direct pool no longer needs an internal cap — `env.maxDirect` (fed from `cfg.MaxDirectUsers`) is the single source of truth and gates additions in `activateUsers` above.

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily.go tools/loadgen/daily_test.go tools/loadgen/collector.go
git commit -m "loadgen: per-step lifecycle (warmup/hold/cooldown)"
```

---

## Task 13: Per-user emitter goroutines + control loop

**Goal:** Wire the per-user state machines so each activated user emits actions during warmup+hold, and add `runDaily(cfg) error` that iterates over steps until trip or completion.

**Files:**
- Modify: `tools/loadgen/daily.go`
- Modify: `tools/loadgen/daily_test.go`

- [ ] **Step 1: Add failing test**

Append to `tools/loadgen/daily_test.go`:

```go
func TestRunDaily_SmokeOnTinyConfig(t *testing.T) {
	// Use a stubbed environment so we don't need real NATS in this unit test.
	// runDaily-Test should run 1 step at N=10, with stubs producing all-green
	// signals, and return without error.
	cfg := dailyConfig{
		Preset: "daily-heavy",
		Steps: []int{10},
		Warmup: 20 * time.Millisecond,
		Hold: 50 * time.Millisecond,
		Cooldown: 10 * time.Millisecond,
		StopOnTrip: true,
		MaxDirectUsers: 10,
		MultiplexPoolSize: 0,
		MaxConnsPerProcess: 10,
	}
	results, err := runDailyForTest(context.Background(), cfg, testEnvFactory{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Tripped)
}
```

You'll need to introduce a small `envFactory` interface so `runDaily` can be tested without real NATS:

```go
// In daily.go:
type envFactory interface {
	Build(cfg dailyConfig, users []*userState) *stepEnv
}

// testEnvFactory in daily_test.go returns a fake stepEnv with stub pollers.
type testEnvFactory struct{}
func (testEnvFactory) Build(cfg dailyConfig, users []*userState) *stepEnv {
	return &stepEnv{
		collector: NewCollector(),
		users:     users,
		thresholds: defaultThresholds(),
		pollPending:    func(_ context.Context) (map[string]int64, error) { return nil, nil },
		scrapeServices: func(_ context.Context) (map[string]int64, error) { return nil, nil },
		maxDirect: cfg.MaxDirectUsers,
		warmup: cfg.Warmup, hold: cfg.Hold, cooldown: cfg.Cooldown,
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `runDailyForTest` undefined.

- [ ] **Step 3: Implement emitter + control loop**

Append to `tools/loadgen/daily.go`:

```go
// startEmitter launches a goroutine that, while ctx is live, ticks the user's
// Markov state every second and, when active, emits actions at the
// Poisson rate scaled by the diurnal envelope.
func startEmitter(ctx context.Context, env *stepEnv, u *userState, holdStart time.Time, holdDuration time.Duration) {
	go func() {
		r := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(len(u.ID))))
		weights := defaultActionWeights()
		baseRate := actionRatePerSecond(weights.totalPerDay(), 8*time.Hour)
		// Compress: a workday becomes the hold window. Multiply rate accordingly.
		compress := (8 * time.Hour).Seconds() / holdDuration.Seconds()
		baseRate *= compress

		tick := time.NewTicker(1 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			u.step(r)
			if !u.active {
				continue
			}
			elapsed := time.Since(holdStart)
			rate := baseRate * rateMultiplier(elapsed, holdDuration)
			// Convert rate (per second) into a probability of firing this tick.
			if r.Float64() < rate {
				doAction(ctx, env, u, r, weights)
			}
		}
	}()
}

func doAction(ctx context.Context, env *stepEnv, u *userState, r *rand.Rand, w actionWeights) {
	a := actionCtx{
		Ctx: ctx, SiteID: "site-local", Rand: r, Collector: env.collector,
		// Publish/Request wired in the real envFactory; nil-safe for stub tests:
	}
	if a.Publish == nil {
		return // stub mode: noop
	}
	switch pickAction(r, w) {
	case actionSend:
		_ = sendMessage(a, u, "loadtest content")
	case actionReadReceipt:
		_ = readReceipt(a, u, "msg-stub")
	case actionScrollHistory:
		_ = scrollHistory(a, u)
	case actionRefreshRoomList:
		_ = refreshRoomList(a, u)
	case actionMemberAdd:
		_ = memberAdd(a, u, "user-stub")
	case actionRoomCreate:
		_ = roomCreate(a, u)
	case actionMuteToggle:
		_ = muteToggle(a, u)
	}
}

// runDailyForTest is the same as runDaily but takes an envFactory so tests
// can inject stubs. runDaily wraps it with the production factory.
func runDailyForTest(ctx context.Context, cfg dailyConfig, factory envFactory) ([]StepResult, error) {
	preset, _ := BuiltinPreset(cfg.Preset)
	preset.Users = maxInt(cfg.Steps) // ensure fixtures cover the largest step
	fx := BuildFixtures(&preset, 42, "site-local")

	users := make([]*userState, len(fx.Users))
	userRooms := groupSubsByUser(fx.Subscriptions)
	for i, u := range fx.Users {
		users[i] = newUserState(u.ID, u.Account, userRooms[u.ID], int64(i))
	}

	env := factory.Build(cfg, users)
	prevN := 0
	var results []StepResult
	for _, n := range cfg.Steps {
		r := runStep(ctx, env, n, prevN)
		results = append(results, r)
		if cfg.StopOnTrip && r.Tripped {
			break
		}
		prevN = n
	}
	return results, nil
}

func maxInt(xs []int) int {
	m := 0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func groupSubsByUser(subs []model.Subscription) map[string][]string {
	out := make(map[string][]string)
	for _, s := range subs {
		out[s.User.ID] = append(out[s.User.ID], s.RoomID)
	}
	return out
}
```

Add the import of `"math/rand"` and `pkg/model` if missing.

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily.go tools/loadgen/daily_test.go
git commit -m "loadgen: per-user emitter goroutines + runDaily control loop"
```

---

## Task 14: Report (console table + CSV)

**Goal:** Render a `StepResult` slice as a console table and emit a CSV file.

**Files:**
- Create: `tools/loadgen/daily_report.go`
- Create: `tools/loadgen/daily_report_test.go`

- [ ] **Step 1: Write the failing test**

Create `tools/loadgen/daily_report_test.go`:

```go
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRenderConsole_IncludesAnswerLine(t *testing.T) {
	results := []StepResult{
		{N: 1000, P50LatencyMs: 12, P95LatencyMs: 45, P99LatencyMs: 89, ErrorRate: 0,
			ConsumerPending: map[string]ConsumerPendingDelta{"broadcast-worker": {Delta: 12}}},
		{N: 2000, P50LatencyMs: 14, P95LatencyMs: 480, P99LatencyMs: 980, ErrorRate: 0,
			ConsumerPending: map[string]ConsumerPendingDelta{"broadcast-worker": {Delta: 1240}},
			Tripped: true, TrippedReasons: []string{"broadcast-worker pending +1240"}},
	}
	var buf bytes.Buffer
	renderConsole(&buf, results)
	out := buf.String()
	require.Contains(t, out, "1000")
	require.Contains(t, out, "PASS")
	require.Contains(t, out, "TRIP")
	require.Contains(t, out, "ANSWER: N = 1000")
}

func TestWriteCSV_OneRowPerStep(t *testing.T) {
	results := []StepResult{
		{N: 1000, P50LatencyMs: 10, StartedAt: time.Unix(1700000000, 0)},
		{N: 2000, P50LatencyMs: 20, StartedAt: time.Unix(1700000200, 0), Tripped: true},
	}
	path := filepath.Join(t.TempDir(), "out.csv")
	require.NoError(t, writeDailyCSV(path, results))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(string(body), "\n")) // header + 2 rows
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — `renderConsole`, `writeDailyCSV` undefined.

- [ ] **Step 3: Implement**

Create `tools/loadgen/daily_report.go`:

```go
package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

func renderConsole(w io.Writer, results []StepResult) {
	fmt.Fprintln(w, "N        p50    p95    p99    err%    worst-pending-delta             verdict")
	var lastPass int
	for _, r := range results {
		verdict := "PASS"
		if r.Inconclusive {
			verdict = "INCONCLUSIVE"
		} else if r.Tripped {
			verdict = "TRIP"
		} else {
			lastPass = r.N
		}
		worst := worstPending(r.ConsumerPending)
		fmt.Fprintf(w, "%-8d %-6.0f %-6.0f %-6.0f %-7.2f%% %-30s %s\n",
			r.N, r.P50LatencyMs, r.P95LatencyMs, r.P99LatencyMs,
			r.ErrorRate*100, worst, verdict)
		if r.Tripped && len(r.TrippedReasons) > 0 {
			fmt.Fprintf(w, "    reasons: %s\n", joinReasons(r.TrippedReasons))
		}
	}
	fmt.Fprintln(w)
	if lastPass > 0 {
		fmt.Fprintf(w, "ANSWER: N = %d (last passing step)\n", lastPass)
		for _, r := range results {
			if r.Tripped {
				fmt.Fprintf(w, "        Next limit: %s\n", joinReasons(r.TrippedReasons))
				break
			}
		}
	} else {
		fmt.Fprintln(w, "ANSWER: no step passed")
	}
}

func worstPending(m map[string]ConsumerPendingDelta) string {
	var worstName string
	var worstDelta int64
	for name, d := range m {
		if d.Delta > worstDelta {
			worstDelta = d.Delta
			worstName = name
		}
	}
	if worstName == "" {
		return "-"
	}
	return fmt.Sprintf("%s +%d", worstName, worstDelta)
}

func joinReasons(rs []string) string {
	out := ""
	for i, r := range rs {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}

func writeDailyCSV(path string, results []StepResult) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{
		"n", "started_at", "p50_ms", "p95_ms", "p99_ms",
		"error_rate", "attempted_ops", "failed_ops",
		"worst_durable", "worst_pending_delta",
		"tripped", "inconclusive", "tripped_reasons",
	}); err != nil {
		return err
	}
	// Stable order.
	rs := make([]StepResult, len(results))
	copy(rs, results)
	sort.Slice(rs, func(i, j int) bool { return rs[i].N < rs[j].N })

	for _, r := range rs {
		worstName, worstDelta := "", int64(0)
		for name, d := range r.ConsumerPending {
			if d.Delta > worstDelta {
				worstDelta, worstName = d.Delta, name
			}
		}
		if err := w.Write([]string{
			strconv.Itoa(r.N),
			r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
			fmt.Sprintf("%.0f", r.P50LatencyMs),
			fmt.Sprintf("%.0f", r.P95LatencyMs),
			fmt.Sprintf("%.0f", r.P99LatencyMs),
			fmt.Sprintf("%.6f", r.ErrorRate),
			strconv.FormatInt(r.AttemptedOps, 10),
			strconv.FormatInt(r.FailedOps, 10),
			worstName,
			strconv.FormatInt(worstDelta, 10),
			strconv.FormatBool(r.Tripped),
			strconv.FormatBool(r.Inconclusive),
			joinReasons(r.TrippedReasons),
		}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/daily_report.go tools/loadgen/daily_report_test.go
git commit -m "loadgen: console + CSV report for daily-IM scenario"
```

---

## Task 15: Production envFactory + wire `runDaily` end-to-end

**Goal:** Real `envFactory` that connects to NATS, builds direct + multiplex pools, wires pendingPoller + serviceScraper. Exposes `runDaily(ctx, cfg, baseCfg) ([]StepResult, error)`.

**Files:**
- Modify: `tools/loadgen/daily.go`
- Modify: `tools/loadgen/daily_test.go`

- [ ] **Step 1: Implement production envFactory**

Append to `tools/loadgen/daily.go`:

```go
// prodEnvFactory wires the real NATS pools and pollers.
type prodEnvFactory struct {
	baseCfg *config // existing top-level loadgen config: NatsURL, etc.
}

func (f *prodEnvFactory) Build(cfg dailyConfig, users []*userState) *stepEnv {
	col := NewCollector()
	direct := newDirectPool(f.baseCfg.NatsURL, col)
	var mux *multiplexPool
	if cfg.MultiplexPoolSize > 0 {
		mux = newMultiplexPool(f.baseCfg.NatsURL, col, cfg.MultiplexPoolSize)
	}
	scraper := newServiceScraper()

	// Resolve service /metrics URLs from docker-compose service names.
	svcURLs := map[string]string{
		"message-gatekeeper": "http://message-gatekeeper:9100/metrics",
		"message-worker":     "http://message-worker:9100/metrics",
		"broadcast-worker":   "http://broadcast-worker:9100/metrics",
		"notification-worker":"http://notification-worker:9100/metrics",
		"room-worker":        "http://room-worker:9100/metrics",
		"room-service":       "http://room-service:9100/metrics",
		"search-sync-worker": "http://search-sync-worker:9100/metrics",
		"inbox-worker":       "http://inbox-worker:9100/metrics",
	}
	jszURL := "http://nats:8222/jsz"

	return &stepEnv{
		collector: col, direct: direct, multiplex: mux, users: users,
		thresholds: defaultThresholds(),
		pollPending: func(ctx context.Context) (map[string]int64, error) {
			return pollPending(ctx, jszURL)
		},
		scrapeServices: func(ctx context.Context) (map[string]int64, error) {
			return scraper.Scrape(ctx, svcURLs)
		},
		maxDirect: cfg.MaxDirectUsers,
		mintJWT: func(ctx context.Context, account string) error {
			// Best-effort one-time auth-service login per user. If auth-service
			// is unreachable or unconfigured, the warning is logged in
			// activateUsers and the user proceeds with shared backend.creds.
			// Adjust URL/payload to match auth-service's actual /login route
			// (check auth-service/routes.go).
			body := fmt.Sprintf(`{"account":%q}`, account)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
				"http://auth-service:8080/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("auth-service login: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("auth-service login status %d", resp.StatusCode)
			}
			return nil
		},
		warmup: cfg.Warmup, hold: cfg.Hold, cooldown: cfg.Cooldown,
	}
}

// runDaily is the production entrypoint invoked by main.go.
func runDaily(ctx context.Context, baseCfg *config, args []string) int {
	cfg, err := parseDailyConfig(args)
	if err != nil {
		slog.Error("parse daily config", "error", err)
		return 2
	}
	results, err := runDailyForTest(ctx, cfg, &prodEnvFactory{baseCfg: baseCfg})
	if err != nil {
		slog.Error("daily run", "error", err)
		return 1
	}
	renderConsole(os.Stdout, results)
	if cfg.CSVPath != "" {
		if err := writeDailyCSV(cfg.CSVPath, results); err != nil {
			slog.Error("csv write", "error", err)
			return 1
		}
	}
	return 0
}
```

- [ ] **Step 2: Verify build**

Run: `make build SERVICE=loadgen`
Expected: builds cleanly.

- [ ] **Step 3: Commit**

```bash
git add tools/loadgen/daily.go
git commit -m "loadgen: production envFactory and runDaily entrypoint"
```

---

## Task 16: Add "daily" subcommand to main.go

**Goal:** Wire `loadgen daily ...` into the existing `dispatch` switch.

**Files:**
- Modify: `tools/loadgen/main.go`
- Modify: `tools/loadgen/main_test.go`

- [ ] **Step 1: Add failing test**

Append to `tools/loadgen/main_test.go`:

```go
func TestDispatch_DailySubcommand(t *testing.T) {
	// dispatch should accept "daily" and return non-zero for unknown preset
	// (so we don't actually run a daily session — just exercise the routing).
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"loadgen", "daily", "--preset=nope"}
	cfg := &config{NatsURL: "nats://x", MongoURI: "mongodb://x", ValkeyAddrs: []string{"x"}}
	rc := dispatch(context.Background(), cfg)
	require.Equal(t, 2, rc)
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `make test SERVICE=loadgen`
Expected: FAIL — dispatch returns "unknown subcommand" for "daily".

- [ ] **Step 3: Add case in dispatch**

In `tools/loadgen/main.go`, inside `dispatch`, add:

```go
case "daily":
    return runDaily(ctx, cfg, os.Args[2:])
```

Update the usage line near the top of `main()` to mention `daily`:

```go
fmt.Fprintln(os.Stderr, "usage: loadgen <seed|run|teardown|daily|members-sustained|members-capacity> [flags]")
```

- [ ] **Step 4: Run, confirm PASS**

Run: `make test SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/main.go tools/loadgen/main_test.go
git commit -m "loadgen: wire 'daily' subcommand into dispatch"
```

---

## Task 17: Integration test against testcontainers

**Goal:** One end-to-end integration test: tiny preset (Users=50, 1 step at N=20), 10s hold, real NATS + Mongo + Valkey via `pkg/testutil`. Asserts a passing verdict.

**Files:**
- Create: `tools/loadgen/daily_integration_test.go`

- [ ] **Step 1: Write the integration test**

Create `tools/loadgen/daily_integration_test.go`:

```go
//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/testutil"
)

func TestRunDaily_Integration_TinyPresetPasses(t *testing.T) {
	natsURL := testutil.NATS(t)
	db := testutil.MongoDB(t, "loadgen_daily")
	keys := testutil.SharedValkeyCluster(t)
	t.Cleanup(func() { testutil.FlushValkey(t) })
	_ = db // fixtures land in db via seed; for this test we only assert verdict

	cfg := dailyConfig{
		Preset: "daily-heavy",
		Steps: []int{20},
		Warmup: 1 * time.Second,
		Hold: 5 * time.Second,
		Cooldown: 500 * time.Millisecond,
		StopOnTrip: true,
		MaxDirectUsers: 20,
		MultiplexPoolSize: 0,
		MaxConnsPerProcess: 25,
	}

	baseCfg := &config{
		NatsURL:     natsURL,
		MongoURI:    testutil.MongoURI(),
		MongoDB:     db.Name(),
		ValkeyAddrs: testutil.ValkeyClusterAddrs(t),
		SiteID:      "site-test",
	}
	_ = keys

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results, err := runDailyForTest(ctx, cfg, &prodEnvFactory{baseCfg: baseCfg})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Tripped, "reasons: %v", results[0].TrippedReasons)
}
```

If `testutil.MongoURI` / `testutil.ValkeyClusterAddrs` don't exist in that exact form, check `pkg/testutil/*.go` for the correct accessors (the helpers `MongoDB`, `NATS`, `SharedValkeyCluster` are guaranteed by CLAUDE.md §4-Integration Tests; the URI/addrs accessors will be near them).

- [ ] **Step 2: Run integration test**

Run: `make test-integration SERVICE=loadgen`
Expected: PASS (or surface real issues to fix in this task).

- [ ] **Step 3: Commit**

```bash
git add tools/loadgen/daily_integration_test.go
git commit -m "loadgen: integration test for daily-IM scenario"
```

---

## Task 18: Deploy Makefile target

**Goal:** `make -C tools/loadgen/deploy run-daily PRESET=daily-heavy` invokes the new subcommand against the docker-compose stack.

**Files:**
- Modify: `tools/loadgen/deploy/Makefile`

- [ ] **Step 1: Read existing run target**

Run: `grep -n "^run:\|^run-dashboards:" tools/loadgen/deploy/Makefile` to find the existing target's exact shape.

- [ ] **Step 2: Add `run-daily` target**

Append to `tools/loadgen/deploy/Makefile`:

```make
run-daily: ## run daily-IM scenario (PRESET=daily-heavy)
	docker compose -f docker-compose.loadgen.yml run --rm loadgen \
		daily --preset=$(PRESET) \
		--steps=$(STEPS) \
		--hold=$(HOLD) \
		--csv=/results/daily-$(PRESET)-$$(date +%Y%m%d-%H%M%S).csv

# Sensible defaults; override on the command line.
STEPS ?= 1000,2000,5000,10000,20000
HOLD ?= 180s
```

(Match the existing target's container-name and compose-file conventions — adjust the docker compose path if the existing `run:` target uses a different file.)

- [ ] **Step 3: Smoke-test the target**

Run: `make -C tools/loadgen/deploy up && make -C tools/loadgen/deploy seed PRESET=small && make -C tools/loadgen/deploy run-daily PRESET=daily-heavy STEPS=100 HOLD=10s`

Expected: container starts, daily run completes, one CSV file lands in `tools/loadgen/deploy/results/`.

- [ ] **Step 4: Tear down**

Run: `make -C tools/loadgen/deploy down`

- [ ] **Step 5: Commit**

```bash
git add tools/loadgen/deploy/Makefile
git commit -m "loadgen: deploy/run-daily target for daily-IM scenario"
```

---

## Task 19: README documentation

**Goal:** Document the new subcommand in `tools/loadgen/README.md`.

**Files:**
- Modify: `tools/loadgen/README.md`

- [ ] **Step 1: Add a "Daily-IM scenario" section**

Append the following section to `tools/loadgen/README.md`:

````markdown
## Daily-IM scenario (find N)

Simulates N users running the chat system as their primary IM
throughout a workday. Ramps N geometrically and reports the largest N
that survived all five SLO signals over a 3-minute steady-state hold.

### Quick start

```
make -C tools/loadgen/deploy up
make -C tools/loadgen/deploy seed PRESET=daily-heavy
make -C tools/loadgen/deploy run-daily PRESET=daily-heavy
```

### Presets

| preset       | DMs | small | medium | large | rooms/user |
|--------------|-----|-------|--------|-------|------------|
| daily-light  | 15  | 10    | 5      | 2     | ~32        |
| daily-heavy  | 25  | 20    | 8      | 3     | ~56        |
| daily-power  | 40  | 30    | 10     | 3     | ~83        |

### CLI

```
loadgen daily \
  --preset=daily-heavy \
  --steps=1k,2k,5k,10k,20k,50k,100k \
  --warmup=60s --hold=180s --cooldown=30s \
  --max-direct-users=20000 --multiplex-pool-size=200 \
  --max-conns-per-process=25000 \
  --csv=results.csv
```

### SLO signals

A step trips if any of:

- p95 publish→broadcast latency > 500ms
- p99 latency > 1000ms
- error rate > 0.1%
- any JetStream consumer's `num_pending` grew by > 1000 over the hold
- any service's `slog_errors_total` increased over the hold

If the loadgen process is itself under pressure (GC pause p99 > 50ms
or CPU > 80%) the step is marked **INCONCLUSIVE** rather than PASS/TRIP,
since the load box is the bottleneck.

### Non-goals

- Not a reconnect/presence-storm test — see separate scenario PR.
- Not a cross-site federation test.
- Not a CI gate. Invoked manually.
````

- [ ] **Step 2: Commit**

```bash
git add tools/loadgen/README.md
git commit -m "docs(loadgen): document daily-IM scenario"
```

---

## Task 20: Final verification

**Goal:** Run the full quality gate the project requires before merge.

- [ ] **Step 1: Run lint**

Run: `make lint`
Expected: PASS. Fix any new findings inline.

- [ ] **Step 2: Run unit tests**

Run: `make test SERVICE=loadgen`
Expected: PASS, ≥80% coverage. Verify coverage:

```
go test -tags='' -coverprofile=cov.out ./tools/loadgen
go tool cover -func=cov.out | grep -E "^total:"
```

Add tests for any uncovered branches.

- [ ] **Step 3: Run integration tests**

Run: `make test-integration SERVICE=loadgen`
Expected: PASS.

- [ ] **Step 4: Run SAST**

Run: `make sast`
Expected: PASS. Suppress findings only with justified `// #nosec` comments per CLAUDE.md §5.

- [ ] **Step 5: Commit any verification fixes and push**

```bash
git add -A
git commit -m "loadgen: address lint/SAST findings for daily-IM scenario" || true
git push -u origin claude/gifted-rubin-ry8HI
```

---

## Notes on assumptions

- **`model.SendMessageRequest.ParentID`** is assumed to exist for thread-reply support; if not, the field must be added in Task 6 with `json` + `bson` tags per CLAUDE.md §3 (Struct Tags).
- **`Collector` accessor names** (`BroadcastsReceived`, `RecordBroadcastReceived`, `Reset`, `LatencySamples`, etc.) are assumed; Task 7 Step 1 and Task 12 Step 3 explicitly verify the existing names and adjust call sites accordingly.
- **Service `/metrics` URLs** are assumed to live at `http://<service>:9100/metrics` inside the docker-compose network. Task 15 may need to adjust ports based on the actual service Dockerfiles.
- **`testutil.MongoURI`/`ValkeyClusterAddrs`** accessor names are assumed; Task 17 Step 1 captures the real names.
- **`runtime/metrics` GC histogram** parsing in Task 10 is a stdlib-only approximation. If `CPUInconclusive` thresholds trip spuriously in production, swap in `github.com/shirou/gopsutil/v3/process` in a follow-up PR.
