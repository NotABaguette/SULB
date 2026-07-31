// Package score normalizes link metrics into a 0-100 quality score.
package score

import "time"

type Weights struct {
	Latency   float64
	Loss      float64
	Bandwidth float64
}

type Metrics struct {
	Latency        time.Duration
	LatencyValid   bool
	Loss           float64 // 0..1
	Bandwidth      float64 // bytes/sec
	BandwidthValid bool
}

// Norm is the normalization mapping. LatencyBest maps to 100, LatencyWorst
// to 0; BandwidthCap (bytes/sec) maps to 100.
type Norm struct {
	LatencyBest  time.Duration
	LatencyWorst time.Duration
	BandwidthCap float64
}

// USER: score normalization lives here — the one place to reshape the
// bandwidth-vs-latency trade-off. Current: linear for all three metrics
// with clamping. Alternatives: log-scale bandwidth (cap becomes the
// ceiling, mid-range links score higher), exponential latency penalty
// (punishes >worst harder), or a piecewise "good enough" curve that
// treats anything under ~50ms as 100. Change the three helpers below,
// nothing else consumes them.
func Score(m Metrics, w Weights, n Norm) float64 {
	lat := 0.0
	if m.LatencyValid {
		ms := float64(m.Latency) / float64(time.Millisecond)
		best := float64(n.LatencyBest) / float64(time.Millisecond)
		worst := float64(n.LatencyWorst) / float64(time.Millisecond)
		lat = clamp01((worst-ms)/(worst-best)) * 100
	}
	loss := (1 - m.Loss) * 100
	bw := 0.0
	if m.BandwidthValid {
		bw = clamp01(m.Bandwidth/n.BandwidthCap) * 100
	}
	// No renormalization: an unmeasured metric contributes 0. A link
	// without a bandwidth probe caps at (w.Latency+w.Loss) of 100 — it
	// ranks below links with proven bandwidth, which is what we want:
	// renormalizing lets an unmeasured link score a flat 100, and then no
	// measured link can ever beat it by the hysteresis margin.
	total := w.Latency + w.Loss + w.Bandwidth
	if total <= 0 {
		return 0
	}
	return (w.Latency*lat + w.Loss*loss + w.Bandwidth*bw) / total
}

// Ewma smooths prev toward next. First sample returns next immediately.
func Ewma(prev, next, alpha float64) float64 {
	if prev == 0 {
		return next
	}
	return alpha*next + (1-alpha)*prev
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
