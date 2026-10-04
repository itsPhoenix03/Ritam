package tui

import "time"

// Throughput metrics.
//
// The design's throughput pane wants three series: tool calls per minute,
// errors per minute and tokens per minute. Two of those the TUI can count for
// itself, because every tool invocation and every failure already arrives as
// an event it is subscribed to. The third cannot: token usage lives in the
// agent CLIs' result envelopes and no adapter surfaces it yet, so the pane
// renders that row as explicitly unavailable rather than inventing a number.
// A plausible-looking fabricated metric is worse than a blank one -- it gets
// believed.

// metricsBuckets is the number of sample buckets in the rolling window and
// metricsBucket is how much time each one covers: twelve five-second buckets,
// i.e. the 60-second window the pane advertises.
const (
	metricsBuckets = 12
	metricsBucket  = 5 * time.Second
	metricsWindow  = metricsBuckets * metricsBucket
)

// metrics is a fixed-size ring of counters over a rolling window.
//
// It is a ring rather than a slice of timestamps so that memory is constant
// regardless of how busy a run gets: a swarm emitting thousands of tool calls
// a minute costs the same as an idle one.
type metrics struct {
	tools  [metricsBuckets]int
	errors [metricsBuckets]int
	// head is the ring index of the bucket that `at` falls in.
	head int
	at   time.Time
}

// advance rolls the ring forward to time t, zeroing any buckets that have
// elapsed since the last observation. Buckets are cleared on advance rather
// than on a timer so an idle UI does no work at all.
func (m *metrics) advance(t time.Time) {
	if m.at.IsZero() {
		m.at = t.Truncate(metricsBucket)
		return
	}
	steps := int(t.Sub(m.at) / metricsBucket)
	if steps <= 0 {
		return
	}
	if steps > metricsBuckets {
		steps = metricsBuckets
	}
	for i := 0; i < steps; i++ {
		m.head = (m.head + 1) % metricsBuckets
		m.tools[m.head] = 0
		m.errors[m.head] = 0
	}
	m.at = m.at.Add(time.Duration(steps) * metricsBucket)
}

// recordTool counts one tool invocation at time t.
func (m *metrics) recordTool(t time.Time) {
	m.advance(t)
	m.tools[m.head]++
}

// recordError counts one failure at time t.
func (m *metrics) recordError(t time.Time) {
	m.advance(t)
	m.errors[m.head]++
}

// series returns the window's buckets oldest-first, ready for a sparkline.
func (m *metrics) series(ring *[metricsBuckets]int) []int {
	out := make([]int, 0, metricsBuckets)
	for i := 1; i <= metricsBuckets; i++ {
		out = append(out, ring[(m.head+i)%metricsBuckets])
	}
	return out
}

// toolSeries and errorSeries are the two wired-up series.
func (m *metrics) toolSeries() []int  { return m.series(&m.tools) }
func (m *metrics) errorSeries() []int { return m.series(&m.errors) }

// total sums a ring, i.e. the per-minute rate the window covers.
func total(ring []int) int {
	n := 0
	for _, v := range ring {
		n += v
	}
	return n
}
