package score

import (
	"testing"
	"time"
)

var norm = Norm{
	LatencyBest:  10 * time.Millisecond,
	LatencyWorst: 300 * time.Millisecond,
	BandwidthCap: 10 << 20,
}

var w = Weights{Latency: 0.5, Loss: 0.2, Bandwidth: 0.3}

func TestLatencyNormalization(t *testing.T) {
	// Loss always participates in renormalization (weights sum 0.7 when
	// bandwidth is invalid): latency 100 -> (0.5*100 + 0.2*100)/0.7 = 100;
	// latency 0   -> (0.5*0   + 0.2*100)/0.7 = 28.57.
	cases := []struct {
		ms   time.Duration
		want float64
	}{
		{10 * time.Millisecond, 100},
		{300 * time.Millisecond, 28.57},
		{155 * time.Millisecond, 64.29},
		{5 * time.Millisecond, 100}, // clamped
		{10 * time.Second, 28.57},   // clamped
	}
	for _, c := range cases {
		got := Score(Metrics{Latency: c.ms, LatencyValid: true}, w, norm)
		if abs(got-c.want) > 0.01 {
			t.Fatalf("latency %v: got %v want %v", c.ms, got, c.want)
		}
	}
}

func TestLossAndBandwidth(t *testing.T) {
	// 10% loss with no latency/bandwidth valid: loss term alone, renormalized.
	got := Score(Metrics{Loss: 0.1}, w, norm)
	if abs(got-90) > 0.01 {
		t.Fatalf("loss-only: got %v want 90", got)
	}
	// Bandwidth at half cap with latency valid too.
	m := Metrics{Latency: 10 * time.Millisecond, LatencyValid: true, Loss: 0, Bandwidth: 5 << 20, BandwidthValid: true}
	got = Score(m, w, norm)
	// latency 100 * 0.5 + loss 100 * 0.2 + bw 50 * 0.3, over sum 1.0
	if abs(got-85) > 0.01 {
		t.Fatalf("mixed: got %v want 85", got)
	}
}

func TestWeightsShiftPreference(t *testing.T) {
	fast := Metrics{Latency: 15 * time.Millisecond, LatencyValid: true, Loss: 0, Bandwidth: 2 << 20, BandwidthValid: true}
	wide := Metrics{Latency: 150 * time.Millisecond, LatencyValid: true, Loss: 0, Bandwidth: 9 << 20, BandwidthValid: true}
	latencyHeavy := Weights{Latency: 0.8, Loss: 0.1, Bandwidth: 0.1}
	bandwidthHeavy := Weights{Latency: 0.1, Loss: 0.1, Bandwidth: 0.8}
	if Score(fast, latencyHeavy, norm) <= Score(wide, latencyHeavy, norm) {
		t.Fatal("latency-heavy weights should prefer the fast link")
	}
	if Score(wide, bandwidthHeavy, norm) <= Score(fast, bandwidthHeavy, norm) {
		t.Fatal("bandwidth-heavy weights should prefer the wide link")
	}
}

func TestEwma(t *testing.T) {
	if got := Ewma(0, 80, 0.3); got != 80 {
		t.Fatalf("first sample: got %v want 80", got)
	}
	// 80 then 40 with alpha 0.3: 0.3*40 + 0.7*80
	if got := Ewma(80, 40, 0.3); abs(got-68) > 0.01 {
		t.Fatalf("ewma: got %v want 68", got)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
