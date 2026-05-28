package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

// HistoryEndpointStat is the per-endpoint aggregate for the summary table.
type HistoryEndpointStat struct {
	Endpoint   HistoryEndpoint
	Count      int
	Latency    Percentiles
	PayloadP50 int
	PayloadP95 int
}

// HistorySummary is the end-of-run report for history-sustained.
type HistorySummary struct {
	Preset, Site     string
	Seed             int64
	TargetRate       int
	ActualRate       float64
	Duration, Warmup time.Duration
	Sent             int
	SentMeasured     int
	Mix              EndpointMix
	BeforeMode       BeforeMode
	PageLimit        int
	Endpoints        []HistoryEndpointStat
	Timeouts         int
	ReplyErrors      int
	BadReplies       int
	NoThreadParents  int
	Saturation       int
	// Bucket-walk-depth classification (LoadHistory replies only).
	SingleBucketReplies int
	MultiBucketReplies  int
}

// PrintHistorySummary writes the end-of-run summary to w in the same shape as
// PrintMembersSummary so log scrapers built for that workload keep working.
func PrintHistorySummary(w io.Writer, s *HistorySummary) error {
	fmt.Fprintln(w, "=== loadgen history-sustained complete ===")
	fmt.Fprintf(w, "preset: %s    seed: %d    site: %s\n", s.Preset, s.Seed, s.Site)
	fmt.Fprintf(w, "duration: %s (warmup: %s, measured: %s)\n",
		s.Duration, s.Warmup, s.Duration-s.Warmup)
	fmt.Fprintf(w, "target rate: %d req/s    actual rate: %.1f req/s\n", s.TargetRate, s.ActualRate)
	totalMix := s.Mix.total()
	totalBefore := s.BeforeMode.total()
	if totalMix > 0 {
		fmt.Fprintf(w, "mix: history=%d%% thread=%d%%\n",
			percent(s.Mix.History, totalMix), percent(s.Mix.Thread, totalMix))
	}
	if totalBefore > 0 {
		fmt.Fprintf(w, "before-mode: open=%d%% scrollback=%d%%\n",
			percent(s.BeforeMode.Open, totalBefore), percent(s.BeforeMode.Scrollback, totalBefore))
	}
	fmt.Fprintf(w, "page-limit: %d\n", s.PageLimit)

	fmt.Fprintln(w, "\nrequest results")
	fmt.Fprintf(w, "  sent (total):     %d\n", s.Sent)
	fmt.Fprintf(w, "  sent (measured):  %d\n", s.SentMeasured)
	fmt.Fprintf(w, "  errors (timeout): %d\n", s.Timeouts)
	fmt.Fprintf(w, "  errors (reply):   %d\n", s.ReplyErrors)
	fmt.Fprintf(w, "  errors (bad):     %d\n", s.BadReplies)
	fmt.Fprintf(w, "  no-thread-parents:%d\n", s.NoThreadParents)
	fmt.Fprintf(w, "  saturation:       %d\n", s.Saturation)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nper-endpoint latency (measured window)")
	fmt.Fprintln(tw, "endpoint\tcount\tp50\tp95\tp99\tmax\tpayload p50\tpayload p95")
	for i := range s.Endpoints {
		e := &s.Endpoints[i]
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%d\t%d\n",
			endpointDisplay(e.Endpoint), e.Count,
			e.Latency.P50, e.Latency.P95, e.Latency.P99, e.Latency.Max,
			e.PayloadP50, e.PayloadP95)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush latency table: %w", err)
	}

	if s.SingleBucketReplies+s.MultiBucketReplies > 0 {
		total := s.SingleBucketReplies + s.MultiBucketReplies
		fmt.Fprintln(w, "\nbucket-walk depth (LoadHistory only)")
		fmt.Fprintf(w, "  single-bucket replies: %d (%.1f%%)\n",
			s.SingleBucketReplies, 100*float64(s.SingleBucketReplies)/float64(total))
		fmt.Fprintf(w, "  multi-bucket replies:  %d (%.1f%%)\n",
			s.MultiBucketReplies, 100*float64(s.MultiBucketReplies)/float64(total))
	}
	return nil
}

func endpointDisplay(e HistoryEndpoint) string {
	switch e {
	case HistoryEndpointThread:
		return "GetThreadMessages"
	default:
		return "LoadHistory"
	}
}

func percent(part, total int) int {
	if total <= 0 {
		return 0
	}
	return part * 100 / total
}

// buildEndpointStats turns a collector's sample tapes into per-endpoint stats
// for the summary. Returned in deterministic order (history, thread).
func buildEndpointStats(c *HistoryCollector) []HistoryEndpointStat {
	historyStats := statsFromSamples(c.HistorySamples(), HistoryEndpointHistory)
	threadStats := statsFromSamples(c.ThreadSamples(), HistoryEndpointThread)
	return []HistoryEndpointStat{historyStats, threadStats}
}

func statsFromSamples(samples []HistorySample, endpoint HistoryEndpoint) HistoryEndpointStat {
	if len(samples) == 0 {
		return HistoryEndpointStat{Endpoint: endpoint}
	}
	latencies := make([]time.Duration, len(samples))
	payloads := make([]int, len(samples))
	for i := range samples {
		latencies[i] = samples[i].Latency
		payloads[i] = samples[i].PayloadBytes
	}
	return HistoryEndpointStat{
		Endpoint:   endpoint,
		Count:      len(samples),
		Latency:    ComputePercentiles(latencies),
		PayloadP50: intPercentile(payloads, 0.50),
		PayloadP95: intPercentile(payloads, 0.95),
	}
}

// intPercentile returns the nearest-rank percentile of vs. vs is sorted in place.
func intPercentile(vs []int, p float64) int {
	if len(vs) == 0 {
		return 0
	}
	sort.Ints(vs)
	idx := int(p * float64(len(vs)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vs) {
		idx = len(vs) - 1
	}
	return vs[idx]
}

// classifyBucketDepth counts LoadHistory samples whose PageDepthMs is within
// one bucket window vs spans multiple buckets. Reflects how often a single
// page required walking past a bucket boundary.
func classifyBucketDepth(c *HistoryCollector, bucketMs int64) (single, multi int) {
	for _, s := range c.HistorySamples() {
		if s.PageDepthMs == 0 {
			// Open-mode replies have no PageDepthMs; skip rather than
			// double-count them as single-bucket.
			continue
		}
		if s.PageDepthMs <= bucketMs {
			single++
		} else {
			multi++
		}
	}
	return single, multi
}

// writeHistoryCSV emits one row per sample with the per-call detail. Row
// schema: timestamp_ms, endpoint, latency_us, payload_bytes, page_depth_ms.
func writeHistoryCSV(w io.Writer, c *HistoryCollector) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"timestamp_ms", "endpoint", "latency_us", "payload_bytes", "page_depth_ms"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, s := range c.HistorySamples() {
		if err := cw.Write(historyCSVRow(s)); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	for _, s := range c.ThreadSamples() {
		if err := cw.Write(historyCSVRow(s)); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

func historyCSVRow(s HistorySample) []string {
	return []string{
		strconv.FormatInt(s.At.UTC().UnixMilli(), 10),
		string(s.Endpoint),
		strconv.FormatInt(s.Latency.Microseconds(), 10),
		strconv.Itoa(s.PayloadBytes),
		strconv.FormatInt(s.PageDepthMs, 10),
	}
}
