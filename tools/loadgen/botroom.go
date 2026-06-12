package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// botRoomReadReq is the typed request payload for a message-read RPC.
type botRoomReadReq struct {
	MessageID string `json:"messageId"`
}

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

// BotRoomSenderConfig parameterizes a single-step bot sender.
type BotRoomSenderConfig struct {
	Publisher      Publisher
	Collector      *Collector
	Metrics        *Metrics
	Preset         string
	SiteID         string
	BotAccount     string
	Rooms          []string // rooms for this size step
	Size           int
	Rate           int
	MaxInFlight    int
	Content        string
	WarmupDeadline time.Time
}

// BotRoomSender drives a paced open-loop publish across the step's rooms.
// Attempted/Failed are cumulative across the sender's lifetime; the ramp loop
// snapshots them at hold start/end to window per-step.
type BotRoomSender struct {
	cfg         BotRoomSenderConfig
	sizeLabel   string
	subjects    []string           // precomputed per-room publish subjects
	warmupPub   prometheus.Counter // pre-built counter handle for warmup phase
	measuredPub prometheus.Counter // pre-built counter handle for measured phase
	cursor      atomic.Uint64
	attempted   atomic.Int64
	failed      atomic.Int64
	lastMsg     sync.Map // roomID -> string (most recent published message ID)
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
	sizeLabel := strconv.Itoa(cfg.Size)
	subjects := make([]string, len(cfg.Rooms))
	for i, room := range cfg.Rooms {
		subjects[i] = subject.MsgSend(cfg.BotAccount, room, cfg.SiteID)
	}
	return &BotRoomSender{
		cfg:         *cfg,
		sizeLabel:   sizeLabel,
		subjects:    subjects,
		warmupPub:   cfg.Metrics.BotRoomPublished.WithLabelValues(cfg.Preset, "warmup", sizeLabel),
		measuredPub: cfg.Metrics.BotRoomPublished.WithLabelValues(cfg.Preset, "measured", sizeLabel),
	}
}

// Attempted returns the number of measured publish attempts so far.
func (s *BotRoomSender) Attempted() int64 { return s.attempted.Load() }

// Failed returns the number of publish failures so far.
func (s *BotRoomSender) Failed() int64 { return s.failed.Load() }

// LastMsgID returns the most recent message ID the sender published to roomID.
func (s *BotRoomSender) LastMsgID(roomID string) string {
	v, ok := s.lastMsg.Load(roomID)
	if !ok {
		return ""
	}
	return v.(string)
}

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
		s.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("marshal").Inc()
		return
	}
	now := time.Now()
	s.cfg.Collector.RecordPublish(reqID, msgID, now)
	s.attempted.Add(1)

	subj := s.subjects[int(idx)%len(s.subjects)]
	if perr := s.cfg.Publisher.Publish(ctx, subj, data); perr != nil {
		s.cfg.Collector.RecordPublishFailed(reqID, msgID)
		s.failed.Add(1)
		s.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("publish").Inc()
		return
	}
	s.lastMsg.Store(roomID, msgID)
	if !s.cfg.WarmupDeadline.IsZero() && now.Before(s.cfg.WarmupDeadline) {
		s.warmupPub.Inc()
	} else {
		s.measuredPub.Inc()
	}
}

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
	GKFailed    *atomic.Int64
	Thresholds  BotRoomThresholds
}

// runBotRoomStep warms up, holds (polling pending at start/end), evaluates the
// verdict, then cools down. The shared collector is windowed to the hold via
// DiscardBefore(holdStart).
//
//nolint:gocritic // hugeParam: pure-function signature is intentional; the per-step copy cost is negligible.
func runBotRoomStep(ctx context.Context, c botRoomStepConfig) BotRoomStepResult {
	sender := NewBotRoomSender(&BotRoomSenderConfig{
		Publisher: c.Publisher, Collector: c.Collector, Metrics: c.Metrics,
		SiteID: c.SiteID, BotAccount: c.BotAccount, Rooms: c.Rooms,
		Size: c.Size, Rate: c.Rate, MaxInFlight: c.MaxInFlight, Preset: c.Preset,
		WarmupDeadline: time.Now().Add(c.Warmup),
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

	_ = waitOrCancel(ctx, c.Warmup)

	// Hold window starts now: drop warmup samples and snapshot baselines.
	holdStart := time.Now()
	c.Collector.DiscardBefore(holdStart)
	a0, f0 := sender.Attempted(), sender.Failed()+c.GKFailed.Load()
	startPending, _ := pollPending(ctx, c.JSZURL)

	_ = waitOrCancel(ctx, c.Hold)

	endPending, _ := pollPending(ctx, c.JSZURL)
	a1, f1 := sender.Attempted(), sender.Failed()+c.GKFailed.Load()

	// Settle briefly so trailing broadcasts land before the snapshot.
	// Intentional: mirrors the existing runRun/runMembers* 2s drain pattern.
	time.Sleep(time.Second)

	in := botRoomStepInputs{
		Size: c.Size, Rooms: len(c.Rooms), TargetRate: c.Rate,
		HoldSeconds: c.Hold.Seconds(),
		Attempted:   a1 - a0,
		Failed:      f1 - f0,
		E2Samples:   c.Collector.LatencySamples(),
		Self:        snapshotSelfMetrics(),
	}
	if reader != nil {
		in.ReadSamples = reader.SamplesMs()
	}
	if startPending != nil && endPending != nil {
		in.ConsumerPending = diffPending(startPending, endPending)
	}

	sizeLabel := strconv.Itoa(c.Size)
	e2Obs := c.Metrics.BotRoomE2ELatency.WithLabelValues(sizeLabel)
	for _, ms := range in.E2Samples {
		e2Obs.Observe(ms / 1000.0)
	}
	readObs := c.Metrics.BotRoomReadLatency.WithLabelValues(sizeLabel)
	for _, ms := range in.ReadSamples {
		readObs.Observe(ms / 1000.0)
	}

	cancel() // stop sender/reader before cooldown
	wg.Wait()
	_ = waitOrCancel(ctx, c.Cooldown)

	return evaluateBotRoomStep(in, c.Thresholds)
}

// botRoomReader issues message.read RPCs against the step's rooms to exercise
// room-service's O(N) read-floor recompute on a large room.
type botRoomReader struct {
	nc       *nats.Conn
	siteID   string
	account  string
	rooms    []string
	subjects []string // precomputed per-room read subjects
	rate     int
	sender   *BotRoomSender
	cursor   atomic.Uint64

	mu      sync.Mutex
	samples []time.Duration
}

func newBotRoomReader(nc *nats.Conn, siteID, account string, rooms []string, rate int, sender *BotRoomSender) *botRoomReader {
	subjects := make([]string, len(rooms))
	for i, room := range rooms {
		subjects[i] = subject.MessageRead(account, room, siteID)
	}
	return &botRoomReader{nc: nc, siteID: siteID, account: account, rooms: rooms, subjects: subjects, rate: rate, sender: sender}
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
	data, err := json.Marshal(botRoomReadReq{MessageID: lastMsgID})
	if err != nil {
		return
	}
	subj := r.subjects[int(idx)%len(r.subjects)]
	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, rerr := r.nc.RequestWithContext(reqCtx, subj, data)
	cancel()
	if rerr != nil {
		if ctx.Err() == nil {
			r.sender.cfg.Metrics.BotRoomPublishErrors.WithLabelValues("timeout").Inc()
		}
		return
	}
	r.record(time.Since(start))
}
