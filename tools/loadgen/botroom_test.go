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
		return botroomPublishErr
	}
	return nil
}

var botroomPublishErr = &botroomStubErr{}

type botroomStubErr struct{}

func (*botroomStubErr) Error() string { return "stub publish error" }

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

func TestBotRoomReader_RecordsLatency(t *testing.T) {
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

	// Bad: zero rooms-per-size.
	err = ValidateBotRoomSelection(&p, []int{50}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rooms-per-size")
}
