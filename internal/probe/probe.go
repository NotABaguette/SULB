// Package probe measures each link: TCP connect latency through the link,
// loss over a sliding window, and an optional bandwidth download sample.
package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/score"
)

type Engine struct {
	links []*links.Link
	sc    config.ScoringConfig
	log   *slog.Logger
}

func New(ls []*links.Link, s config.ScoringConfig) *Engine {
	return &Engine{links: ls, sc: s, log: slog.Default()}
}

// Start launches one goroutine per link for TCP probes and, when enabled,
// one for bandwidth probes. Stops when ctx is canceled.
func (e *Engine) Start(ctx context.Context) {
	for _, l := range e.links {
		l.SetFloor(e.sc.Floor)
		probeCfg := l.Cfg().Probe
		go e.probeLoop(ctx, l, probeCfg)
		if l.Cfg().BandwidthProbe.Enable {
			go e.bandwidthLoop(ctx, l, l.Cfg().BandwidthProbe)
		}
	}
}

func (e *Engine) probeLoop(ctx context.Context, l *links.Link, cfg config.ProbeConfig) {
	interval := cfg.Interval.Duration
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.probeOnce(ctx, l, cfg)
		}
	}
}

func (e *Engine) probeOnce(ctx context.Context, l *links.Link, cfg config.ProbeConfig) {
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	var latencyMs float64
	oks := 0
	for _, tgt := range cfg.Targets {
		ok, lat := TCPProbe(ctx, l, tgt.Host, tgt.Port, timeout)
		l.RecordProbe(ok, lat)
		if ok {
			oks++
			latencyMs += float64(lat) / float64(time.Millisecond)
		}
	}
	loss := 1.0
	if len(cfg.Targets) > 0 {
		loss = 1 - float64(oks)/float64(len(cfg.Targets))
	}
	m := score.Metrics{
		Loss:         loss,
		LatencyValid: oks > 0,
		Latency:      time.Duration(latencyMs / float64(max(oks, 1)) * float64(time.Millisecond)),
	}
	l.UpdateScore(m)
	if oks < len(cfg.Targets) {
		e.log.Warn("probe degraded", "link", l.Name(), "ok", oks, "of", len(cfg.Targets))
	}
}

// TCPProbe dials host:port through the link and reports success + latency.
func TCPProbe(ctx context.Context, l *links.Link, host string, port uint16, timeout time.Duration) (bool, time.Duration) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false, 0
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	c, err := l.DialContext(dctx, netip.AddrPortFrom(addr, port))
	if err != nil {
		return false, 0
	}
	c.Close()
	return true, time.Since(start)
}

func (e *Engine) bandwidthLoop(ctx context.Context, l *links.Link, cfg config.BandwidthCfg) {
	interval := cfg.Interval.Duration
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bw, err := BandwidthOnce(ctx, l, cfg.URL, cfg.Bytes)
			if err != nil {
				e.log.Warn("bandwidth probe failed", "link", l.Name(), "err", err)
				l.SetBandwidth(0, false)
				continue
			}
			l.SetBandwidth(bw, true)
			e.log.Info("bandwidth probe", "link", l.Name(), "mbps", bw*8/1e6)
		}
	}
}

// BandwidthOnce downloads wantBytes through the link and returns bytes/sec.
func BandwidthOnce(ctx context.Context, l *links.Link, url string, wantBytes int64) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "80"
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return 0, fmt.Errorf("resolve %s: %w", host, err)
	}
	addr, _ := netip.AddrFromSlice(ips[0])
	pn, err := net.LookupPort("tcp", port)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	c, err := l.DialContext(ctx, netip.AddrPortFrom(addr.Unmap(), uint16(pn)))
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if err := req.Write(c); err != nil {
		return 0, err
	}
	read := int64(0)
	buf := make([]byte, 64*1024)
	for read < wantBytes {
		n, err := c.Read(buf)
		read += int64(n)
		if err != nil {
			return 0, err
		}
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0, fmt.Errorf("no elapsed time")
	}
	return float64(read) / elapsed.Seconds(), nil
}
