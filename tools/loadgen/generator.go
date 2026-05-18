package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/subject"
)

// InjectMode selects which subject the generator publishes onto.
type InjectMode string

const (
	InjectFrontdoor InjectMode = "frontdoor"
	InjectCanonical InjectMode = "canonical"
)

// Publisher abstracts NATS publishing so tests can inject a recorder.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// GeneratorConfig is the parameter bundle for a Generator.
// Preset is *Preset because the struct is large enough that gocritic's
// hugeParam rule would flag the embedded value.
type GeneratorConfig struct {
	Preset         *Preset
	Fixtures       Fixtures
	SiteID         string
	Rate           int
	Inject         InjectMode
	Publisher      Publisher
	Metrics        *Metrics
	Collector      *Collector
	WarmupDeadline time.Time
	// MaxInFlight caps concurrent publishes dispatched from the ticker.
	// Set to 0 to publish serially on the ticker goroutine (legacy behavior,
	// useful for bisection).
	MaxInFlight int
}

// Generator is the open-loop publisher.
type Generator struct {
	cfg     GeneratorConfig
	rngMu   sync.Mutex
	rng     *rand.Rand
	maxBody string
}

// NewGenerator returns a Generator seeded from `seed`.
func NewGenerator(cfg *GeneratorConfig, seed int64) *Generator {
	max := cfg.Preset.ContentBytes.Max
	if max <= 0 {
		max = 1
	}
	return &Generator{
		cfg:     *cfg,
		rng:     rand.New(rand.NewSource(seed)),
		maxBody: strings.Repeat("x", max),
	}
}

// drainGracePeriod bounds how long Run waits for in-flight publishes
// to complete after ctx cancels.
const drainGracePeriod = 5 * time.Second

// Run publishes at the configured rate until ctx is cancelled. When
// MaxInFlight > 0, each tick dispatches the publish to a bounded
// goroutine pool so the ticker stays punctual under load; saturation
// (pool full when a tick fires) is recorded as a publish error with
// reason="saturated" rather than silently dropping the tick.
func (g *Generator) Run(ctx context.Context) error {
	if g.cfg.Rate <= 0 {
		return fmt.Errorf("rate must be > 0")
	}
	interval := time.Second / time.Duration(g.cfg.Rate)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	if g.cfg.MaxInFlight <= 0 {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
				g.publishOne(ctx)
			}
		}
	}

	sem := make(chan struct{}, g.cfg.MaxInFlight)
	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(drainGracePeriod):
			}
			return nil
		case <-tick.C:
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer func() {
						<-sem
						wg.Done()
					}()
					g.publishOne(ctx)
				}()
			default:
				g.cfg.Metrics.PublishErrors.WithLabelValues(g.cfg.Preset.Name, "saturated").Inc()
			}
		}
	}
}

// intn returns rng.Intn(n) with mutex protection so publishOne is
// safe to call from multiple worker goroutines.
func (g *Generator) intn(n int) int {
	g.rngMu.Lock()
	defer g.rngMu.Unlock()
	return g.rng.Intn(n)
}

func (g *Generator) float64() float64 {
	g.rngMu.Lock()
	defer g.rngMu.Unlock()
	return g.rng.Float64()
}

func (g *Generator) publishOne(ctx context.Context) {
	if len(g.cfg.Fixtures.Subscriptions) == 0 {
		return
	}
	subIdx := g.intn(len(g.cfg.Fixtures.Subscriptions))
	sub := g.cfg.Fixtures.Subscriptions[subIdx]
	content := g.content()
	msgID := idgen.GenerateMessageID()
	publishTime := time.Now()

	var (
		subj  string
		data  []byte
		reqID string
		err   error
	)
	switch g.cfg.Inject {
	case InjectCanonical:
		now := time.Now().UTC()
		evt := model.MessageEvent{
			Event: model.EventCreated,
			Message: model.Message{
				ID: msgID, RoomID: sub.RoomID,
				UserID: sub.User.ID, UserAccount: sub.User.Account,
				Content: content, CreatedAt: now,
			},
			SiteID:    g.cfg.SiteID,
			Timestamp: now.UnixMilli(),
		}
		data, err = json.Marshal(evt)
		subj = subject.MsgCanonicalCreated(g.cfg.SiteID)
		g.cfg.Collector.RecordPublishBroadcastOnly(msgID, publishTime)
	default:
		reqID = idgen.GenerateRequestID()
		req := model.SendMessageRequest{ID: msgID, Content: content, RequestID: reqID}
		data, err = json.Marshal(req)
		subj = subject.MsgSend(sub.User.Account, sub.RoomID, g.cfg.SiteID)
		g.cfg.Collector.RecordPublish(reqID, msgID, publishTime)
	}
	if err != nil {
		g.cfg.Metrics.PublishErrors.WithLabelValues(g.cfg.Preset.Name, "marshal").Inc()
		return
	}
	if perr := g.cfg.Publisher.Publish(ctx, subj, data); perr != nil {
		g.cfg.Collector.RecordPublishFailed(reqID, msgID)
		g.cfg.Metrics.PublishErrors.WithLabelValues(g.cfg.Preset.Name, "publish").Inc()
		return
	}
	phase := "measured"
	if publishTime.Before(g.cfg.WarmupDeadline) {
		phase = "warmup"
	}
	g.cfg.Metrics.Published.WithLabelValues(g.cfg.Preset.Name, phase).Inc()
}

func (g *Generator) content() string {
	r := g.cfg.Preset.ContentBytes
	size := r.Min
	if r.Max > r.Min {
		size = r.Min + g.intn(r.Max-r.Min+1)
	}
	if size <= 0 {
		size = 1
	}
	body := g.maxBody[:size]
	if g.cfg.Preset.MentionRate > 0 && g.float64() < g.cfg.Preset.MentionRate {
		target := g.intn(g.cfg.Preset.Users)
		body = fmt.Sprintf("@user-%d %s", target, body)
	}
	return body
}
