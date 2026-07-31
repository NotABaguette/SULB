package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/score"
)

func d(x time.Duration) config.Duration { return config.Duration{Duration: x} }

func testNorm() score.Norm {
	return score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	}
}

func directLink(t *testing.T) *links.Link {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: "direct", Type: "direct"}, 0.3, testNorm())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func acceptAndClose(t *testing.T) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr()
}

func TestTCPProbe(t *testing.T) {
	addr := acceptAndClose(t)
	l := directLink(t)
	ap, _ := netip.ParseAddrPort(addr.String())
	ok, lat := TCPProbe(context.Background(), l, ap.Addr().String(), ap.Port(), 2*time.Second)
	if !ok || lat <= 0 {
		t.Fatalf("probe to live listener: ok=%v lat=%v", ok, lat)
	}
	// closed port -> fail
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	ok, _ = TCPProbe(context.Background(), l, "127.0.0.1", mustPort(t, dead), 200*time.Millisecond)
	if ok {
		t.Fatal("probe to closed port must fail")
	}
}

func mustPort(t *testing.T, addr string) uint16 {
	t.Helper()
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return ap.Port()
}

func TestBandwidthOnce(t *testing.T) {
	var payload = make([]byte, 65536)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	l := directLink(t)
	bw, err := BandwidthOnce(context.Background(), l, srv.URL, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if bw <= 0 {
		t.Fatalf("bandwidth must be positive, got %v", bw)
	}
}

func TestEngineRunsProbes(t *testing.T) {
	addr := acceptAndClose(t)
	ap, _ := netip.ParseAddrPort(addr.String())
	l, err := links.New(config.LinkConfig{
		Name:  "direct",
		Type:  "direct",
		Probe: config.ProbeConfig{Targets: []config.ProbeTarget{{Host: ap.Addr().String(), Port: ap.Port()}}, Interval: d(200 * time.Millisecond), Timeout: d(time.Second), LossWindow: 10},
	}, 0.3, testNorm())
	if err != nil {
		t.Fatal(err)
	}
	e := New([]*links.Link{l}, config.ScoringConfig{EWMAAlpha: 0.3, LatencyBest: d(10 * time.Millisecond), LatencyWorst: d(300 * time.Millisecond), BandwidthCap: 10 << 20})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := l.Snapshot()
		if s.Latency > 0 && s.State == links.StateUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probes never landed: %+v", s)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
