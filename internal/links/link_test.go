package links

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/score"
)

func testNorm() score.Norm {
	return score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	}
}

func testLink(t *testing.T, name, typ, endpoint string) *Link {
	t.Helper()
	return testLinkCfg(t, config.LinkConfig{Name: name, Type: typ, Endpoint: endpoint})
}

func testLinkCfg(t *testing.T, cfg config.LinkConfig) *Link {
	t.Helper()
	l, err := New(cfg, 0.3, testNorm())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestStateTransitions(t *testing.T) {
	l := testLink(t, "a", "direct", "")
	if l.Snapshot().State != StateUp {
		t.Fatal("starts Up (optimistic; first probes correct it)")
	}
	// 3 consecutive failures -> Down
	for i := 0; i < 3; i++ {
		l.RecordProbe(false, 0)
	}
	if s := l.Snapshot(); s.State != StateDown {
		t.Fatalf("want Down after 3 fails, got %v", s.State)
	}
	// 2 consecutive successes -> Up again
	l.RecordProbe(true, 20*time.Millisecond)
	if s := l.Snapshot(); s.State != StateDown {
		t.Fatalf("1 success not enough to recover: %v", s.State)
	}
	l.RecordProbe(true, 20*time.Millisecond)
	if s := l.Snapshot(); s.State != StateUp {
		t.Fatalf("want Up after 2 succs, got %v", s.State)
	}
}

func TestLossWindow(t *testing.T) {
	l := testLinkCfg(t, config.LinkConfig{Name: "a", Type: "direct", Probe: config.ProbeConfig{LossWindow: 10}})
	// 3 fails out of 10 -> loss 0.3
	for i := 0; i < 10; i++ {
		l.RecordProbe(i%10 < 7, 10*time.Millisecond) // last 3 fail
	}
	if s := l.Snapshot(); s.Loss < 0.29 || s.Loss > 0.31 {
		t.Fatalf("loss: got %v want ~0.3", s.Loss)
	}
	// window slides: 10 successes -> loss 0
	for i := 0; i < 10; i++ {
		l.RecordProbe(true, 10*time.Millisecond)
	}
	if s := l.Snapshot(); s.Loss != 0 {
		t.Fatalf("loss after recovery: got %v want 0", s.Loss)
	}
}

func TestLatencyEwmaSmoothsSpike(t *testing.T) {
	l := testLink(t, "a", "direct", "")
	l.RecordProbe(true, 10*time.Millisecond)
	l.RecordProbe(true, 200*time.Millisecond) // spike
	l.RecordProbe(true, 10*time.Millisecond)
	if s := l.Snapshot(); s.Latency > 100*time.Millisecond {
		t.Fatalf("ewma should damp the spike: got %v", s.Latency)
	}
}

func TestScoreUpdateAndDegraded(t *testing.T) {
	l := testLink(t, "a", "direct", "")
	l.SetFloor(30)
	l.UpdateScore(score.Metrics{Latency: 10 * time.Millisecond, LatencyValid: true, Loss: 0})
	if s := l.Snapshot(); s.Score < 99 {
		t.Fatalf("near-perfect metrics should score ~100: got %v", s.Score)
	}
	// EWMA decays, not jumps: 100 -> 70 -> 49 after repeated terrible
	// updates (alpha 0.3).
	prev := l.Snapshot().Score
	for i := 0; i < 3; i++ {
		l.UpdateScore(score.Metrics{Latency: 10 * time.Second, LatencyValid: true, Loss: 1})
		cur := l.Snapshot().Score
		if cur >= prev {
			t.Fatalf("score must decay: %v -> %v", prev, cur)
		}
		prev = cur
	}
	if s := l.Snapshot(); s.Score >= 50 {
		t.Fatalf("terrible metrics should decay below 50: got %v", s.Score)
	}
}

func TestDialDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var b [1]byte
				if _, err := c.Read(b[:]); err == nil {
					c.Write(b[:])
				}
			}()
		}
	}()
	l := testLink(t, "a", "direct", "")
	addr, _ := netip.ParseAddrPort(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := l.DialContext(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{0x42}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil || b[0] != 0x42 {
		t.Fatalf("echo: got %x err %v", b, err)
	}
}

func TestSnapshotAndRouting(t *testing.T) {
	l := testLink(t, "a", "direct", "")
	l.SetRoutingOK(false)
	s := l.Snapshot()
	if s.RoutingOK {
		t.Fatal("routing should be false")
	}
	l.SetBandwidth(5<<20, true)
	s = l.Snapshot()
	if !s.BandwidthValid || s.Bandwidth != 5<<20 {
		t.Fatalf("bandwidth not recorded: %+v", s)
	}
}
