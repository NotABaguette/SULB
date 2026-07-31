// Package links defines one uplink (a dialable path) and its health state.
package links

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"sulb/internal/config"
	"sulb/internal/score"

	"github.com/xjasonlyu/tun2socks/v2/dialer"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
)

type State int8

const (
	StateDown State = iota
	StateDegraded
	StateUp
)

func (s State) String() string {
	switch s {
	case StateDown:
		return "down"
	case StateDegraded:
		return "degraded"
	default:
		return "up"
	}
}

// Snapshot is a consistent read of a link's runtime state.
type Snapshot struct {
	Name           string
	Type           string
	State          State
	Score          float64
	Latency        time.Duration
	Loss           float64
	Bandwidth      float64
	BandwidthValid bool
	RoutingOK      bool
}

type Link struct {
	cfg     config.LinkConfig
	alpha   float64
	norm    score.Norm
	weights score.Weights
	socks   *socks5.Socks5 // non-nil only for type socks5

	mu             sync.RWMutex
	state          State
	score          float64
	latencyMs      float64 // EWMA
	bandwidth      float64
	bandwidthValid bool
	routingOK      bool
	floor          float64 // score below this -> DEGRADED; 0 = disabled
	lossFails      int
	succs          int
	ring           []bool
	ringIdx        int
}

func New(cfg config.LinkConfig, alpha float64, norm score.Norm) (*Link, error) {
	// Config.Load applies these defaults, but keep the invariants here too:
	// a zero-weight link would score 0 forever, and a zero recover
	// threshold would flip Down->Up on the first success.
	w := cfg.Weights
	if w == (config.Weights{}) {
		w = config.Weights{Latency: 0.5, Loss: 0.2, Bandwidth: 0.3}
	}
	if cfg.Probe.FailThreshold == 0 {
		cfg.Probe.FailThreshold = 3
	}
	if cfg.Probe.RecoverThreshold == 0 {
		cfg.Probe.RecoverThreshold = 2
	}
	l := &Link{
		cfg:       cfg,
		alpha:     alpha,
		norm:      norm,
		weights:   score.Weights{Latency: w.Latency, Loss: w.Loss, Bandwidth: w.Bandwidth},
		state:     StateUp,
		routingOK: true,
	}
	if cfg.Type == "socks5" {
		s, err := socks5.New(cfg.Endpoint, "", "")
		if err != nil {
			return nil, fmt.Errorf("link %q: bad socks5 endpoint: %w", cfg.Name, err)
		}
		l.socks = s
	}
	return l, nil
}

func (l *Link) Name() string           { return l.cfg.Name }
func (l *Link) Type() string           { return l.cfg.Type }
func (l *Link) Cfg() config.LinkConfig { return l.cfg }
func (l *Link) SetFloor(f float64)     { l.mu.Lock(); defer l.mu.Unlock(); l.floor = f }

// RecordProbe records one probe result and advances the state machine.
// ok=false with latency=0 means the probe failed.
func (l *Link) RecordProbe(ok bool, latency time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ok {
		l.succs++
		l.lossFails = 0
		if l.succs >= l.cfg.Probe.RecoverThreshold && l.state == StateDown {
			l.state = StateUp
		}
		l.latencyMs = score.Ewma(l.latencyMs, float64(latency)/float64(time.Millisecond), l.alpha)
	} else {
		l.lossFails++
		l.succs = 0
		if l.lossFails >= l.cfg.Probe.FailThreshold && l.state != StateDown {
			l.state = StateDown
		}
	}
	l.addRing(ok)
}

// RecordPassive merges the RTT of a real connection into the latency EWMA
// without touching probe counters.
func (l *Link) RecordPassive(latency time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.latencyMs = score.Ewma(l.latencyMs, float64(latency)/float64(time.Millisecond), l.alpha)
}

// UpdateScore recomputes the EWMA score from the latest metrics.
func (l *Link) UpdateScore(m score.Metrics) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw := score.Score(m, l.weights, l.norm)
	l.score = score.Ewma(l.score, raw, l.alpha)
	if m.BandwidthValid {
		l.bandwidth, l.bandwidthValid = m.Bandwidth, true
	}
	if l.floor > 0 {
		if raw < l.floor && l.state == StateUp {
			l.state = StateDegraded
		} else if raw >= l.floor && l.state == StateDegraded {
			l.state = StateUp
		}
	}
}

func (l *Link) SetBandwidth(b float64, valid bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bandwidth, l.bandwidthValid = b, valid
}

func (l *Link) SetRoutingOK(ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.routingOK = ok
}

func (l *Link) Snapshot() Snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	loss := 0.0
	if n := len(l.ring); n > 0 {
		loss = float64(l.lossFailsInRing()) / float64(n)
	}
	return Snapshot{
		Name: l.cfg.Name, Type: l.cfg.Type, State: l.state,
		Score: l.score, Latency: time.Duration(l.latencyMs * float64(time.Millisecond)),
		Loss: loss, Bandwidth: l.bandwidth, BandwidthValid: l.bandwidthValid,
		RoutingOK: l.routingOK,
	}
}

func (l *Link) lossFailsInRing() int {
	n := 0
	for _, ok := range l.ring {
		if !ok {
			n++
		}
	}
	return n
}

func (l *Link) addRing(ok bool) {
	if len(l.ring) == 0 {
		w := l.cfg.Probe.LossWindow
		if w <= 0 {
			w = 10
		}
		l.ring = make([]bool, w)
	}
	l.ring[l.ringIdx] = ok
	l.ringIdx = (l.ringIdx + 1) % len(l.ring)
}

// DialContext opens a TCP connection to dst through this link.
func (l *Link) DialContext(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	switch l.cfg.Type {
	case "socks5":
		return l.socks.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: dst.Addr(), DstPort: dst.Port()})
	case "direct":
		return dialer.DialContext(ctx, "tcp", dst.String())
	default:
		return nil, fmt.Errorf("link %q (%s) cannot dial: router links are route-managed", l.cfg.Name, l.cfg.Type)
	}
}

// DialUDP returns a packet conn to dst through this link.
func (l *Link) DialUDP(dst netip.AddrPort) (net.PacketConn, error) {
	switch l.cfg.Type {
	case "socks5":
		return l.socks.DialUDP(&M.Metadata{Network: M.UDP, DstIP: dst.Addr(), DstPort: dst.Port()})
	case "direct":
		return dialer.ListenPacket("udp", "")
	default:
		return nil, fmt.Errorf("link %q (%s) cannot dial: router links are route-managed", l.cfg.Name, l.cfg.Type)
	}
}
