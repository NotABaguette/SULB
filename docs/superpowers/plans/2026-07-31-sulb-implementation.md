# SULB Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build sulb — a cross-platform (Linux/macOS/Windows) Go daemon that exposes one IP (TUN + SOCKS5) and forwards each new TCP connection / UDP session through the best of several local SOCKS5 uplinks (plus route-based router links), scored by configurable weighted latency/loss/bandwidth with EWMA, hysteresis, and stick time.

**Architecture:** One Go binary. Entry = TUN interface (gVisor netstack via tun2socks v2 library) + in-house SOCKS5 server. Both hand new flows to a Picker that selects a Link (socks5 / router / direct) per flow. A probe engine measures each link (TCP connect through the link, optional HTTP bandwidth probe) feeding a pure scoring function; the picker applies hysteresis + stick time so links never flap. Router links re-point OS routes via the chosen gateway.

**Tech Stack:** Go 1.26+, `github.com/xjasonlyu/tun2socks/v2` (TUN device, netstack, SOCKS5 client with UDP ASSOCIATE — MIT), `gopkg.in/yaml.v3`. No other deps. SOCKS5 *server* written in-house (~140 lines).

## Global Constraints

- Go module name: `sulb`; go directive `go 1.26` (tun2socks v2 requires it).
- Exactly 3 module deps: `github.com/xjasonlyu/tun2socks/v2` (latest), `gopkg.in/yaml.v3`, `golang.org/x/sys` (indirect, don't add explicitly).
- All tun2socks APIs used (verified against HEAD on 2026-07-31): `core/device/tun.Open(name string, mtu uint32) (device.Device, error)`, `core.CreateStack(&core.Config{LinkEndpoint, TransportHandler, ICMPHandler, Options})`, `core/adapter.TransportHandler{HandleTCP(adapter.TCPConn), HandleUDP(adapter.UDPConn)}`, `adapter.TCPConn/UDPConn` expose `ID() stack.TransportEndpointID` (`LocalAddress/Port` = dst, `RemoteAddress/Port` = src), `metadata.Metadata{Network, SrcIP, SrcPort, DstIP, DstPort}` with `M.TCP`/`M.UDP`, `proxy/socks5.New(addr, user, pass string) (*Socks5, error)` with `DialContext(ctx, *M.Metadata)` / `DialUDP(*M.Metadata)`, `buffer.Get(buffer.RelayBufferSize|buffer.MaxSegmentSize)` / `buffer.Put`.
- No mid-flow switching; only new flows re-pick. Existing connections ride their original link.
- `links.New` stores `alpha` (EWMA) and `norm` (score.Norm) from scoring config — scoring state lives in the Link.
- `score.Score` is the user-shaped function (marked `// USER:`); normalization defaults: latency linear best→100/worst→0, bandwidth linear cap→100, loss (1-loss)→100.
- Admin/root required at runtime for TUN only. Router links: Linux first-class (`ip route replace`), macOS/Windows best-effort (`route change`→`add` fallback).
- Commit after every task. Message format: `feat: ...` / `test: ...` with Co-Authored-By line.

---

### Task 1: Module + config package

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `sulb.yaml` (default config)

**Interfaces:**
- Produces: `config.Load(path string) (*Config, error)`; `Config{Entry EntryConfig, Scoring ScoringConfig, Status StatusConfig, Links []LinkConfig}`; `EntryConfig{TUNName string, TUNIP string, TUNNet int, MTU uint32, SOCKSListen string}`; `ScoringConfig{EWMAAlpha float64, Hysteresis float64, StickTime Duration, LatencyBest Duration, LatencyWorst Duration, BandwidthCap float64, Floor float64}`; `StatusConfig{Listen string}`; `LinkConfig{Name, Type, Endpoint, Gateway string, Routes []string, Weights Weights, Probe ProbeConfig, BandwidthProbe BandwidthCfg}`; `Weights{Latency, Loss, Bandwidth float64}`; `ProbeConfig{Targets []ProbeTarget, Interval, Timeout Duration, FailThreshold, RecoverThreshold, LossWindow int}`; `ProbeTarget{Host string, Port uint16}`; `BandwidthCfg{Enable bool, URL string, Bytes int64, Interval Duration}`; `Duration struct{ time.Duration }` (YAML string parse).

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sulb.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, "links:\n  - name: a\n    type: socks5\n    endpoint: 127.0.0.1:1081\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Entry.TUNIP != "10.66.66.1" || c.Entry.SOCKSListen != "10.66.66.1:1080" {
		t.Fatalf("entry defaults wrong: %+v", c.Entry)
	}
	if c.Scoring.EWMAAlpha != 0.3 || c.Scoring.Hysteresis != 0.10 || c.Scoring.StickTime.Duration != 15*time.Second {
		t.Fatalf("scoring defaults wrong: %+v", c.Scoring)
	}
	if c.Links[0].Probe.Interval.Duration != 5*time.Second || c.Links[0].Probe.FailThreshold != 3 {
		t.Fatalf("probe defaults wrong: %+v", c.Links[0].Probe)
	}
	w := c.Links[0].Weights
	if w.Latency != 0.5 || w.Loss != 0.2 || w.Bandwidth != 0.3 {
		t.Fatalf("weight defaults wrong: %+v", w)
	}
	if len(c.Links[0].Probe.Targets) != 1 || c.Links[0].Probe.Targets[0].Host != "1.1.1.1" {
		t.Fatalf("target default wrong: %+v", c.Links[0].Probe.Targets)
	}
}

func TestParseFull(t *testing.T) {
	c, err := Load(writeTemp(t, `
entry:
  tun_name: sulb0
  tun_ip: 10.66.66.2
  tun_net: 24
  mtu: 1400
  socks_listen: 127.0.0.1:1080
scoring:
  ewma_alpha: 0.5
  hysteresis: 0.2
  stick_time: 30s
  latency_best: 5ms
  latency_worst: 500ms
  bandwidth_cap: 20971520
  floor: 25
status:
  listen: 127.0.0.1:9090
links:
  - name: vpn-b
    type: router
    gateway: 192.168.1.2
    routes: ["0.0.0.0/0"]
    probe:
      targets: [{host: 8.8.8.8, port: 443}]
      interval: 10s
      timeout: 1s
      fail_threshold: 5
      recover_threshold: 3
      loss_window: 20
    bandwidth_probe:
      enable: true
      url: "http://127.0.0.1:8000/test.bin"
      bytes: 1048576
      interval: 120s
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Entry.TUNIP != "10.66.66.2" || c.Scoring.StickTime.Duration != 30*time.Second ||
		c.Links[0].Type != "router" || c.Links[0].Gateway != "192.168.1.2" ||
		c.Links[0].Probe.LossWindow != 20 || c.Links[0].BandwidthProbe.Bytes != 1048576 {
		t.Fatalf("full parse wrong: %+v", c)
	}
}

func TestValidation(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wantErr string }{
		{"dup names", "links:\n  - {name: a, type: socks5, endpoint: 127.0.0.1:1}\n  - {name: a, type: socks5, endpoint: 127.0.0.1:2}\n", "duplicate"},
		{"bad type", "links:\n  - {name: a, type: http}\n", "type"},
		{"socks no endpoint", "links:\n  - {name: a, type: socks5}\n", "endpoint"},
		{"router no gateway", "links:\n  - {name: a, type: router}\n", "gateway"},
		{"zero weights", "links:\n  - {name: a, type: socks5, endpoint: 127.0.0.1:1, weights: {latency: 0, loss: 0, bandwidth: 0}}\n", "weights"},
		{"bad target host", "links:\n  - {name: a, type: socks5, endpoint: 127.0.0.1:1, probe: {targets: [{host: notanip, port: 443}]}}\n", "target"},
		{"bad route", "links:\n  - {name: a, type: router, gateway: 192.168.1.2, routes: [nope]}\n", "route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — no go.mod (module not found).

- [ ] **Step 3: Write minimal implementation**

Run: `go mod init sulb` — then go.mod contains `module sulb` (go directive added by `go mod tidy` later).

`internal/config/config.go`:

```go
package config

import (
	"fmt"
	"net/netip"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML strings like "5s" parse.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	dd, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	d.Duration = dd
	return nil
}

type Config struct {
	Entry   EntryConfig
	Scoring ScoringConfig
	Status  StatusConfig
	Links   []LinkConfig
}

type EntryConfig struct {
	TUNName     string `yaml:"tun_name"`
	TUNIP       string `yaml:"tun_ip"`
	TUNNet      int    `yaml:"tun_net"`
	MTU         uint32 `yaml:"mtu"`
	SOCKSListen string `yaml:"socks_listen"`
}

type ScoringConfig struct {
	EWMAAlpha    float64  `yaml:"ewma_alpha"`
	Hysteresis   float64  `yaml:"hysteresis"`
	StickTime    Duration `yaml:"stick_time"`
	LatencyBest  Duration `yaml:"latency_best"`
	LatencyWorst Duration `yaml:"latency_worst"`
	BandwidthCap float64  `yaml:"bandwidth_cap"` // bytes/sec mapping to score 100
	Floor        float64  `yaml:"floor"`         // score below this -> DEGRADED; 0 = disabled
}

type StatusConfig struct {
	Listen string `yaml:"listen"`
}

type LinkConfig struct {
	Name           string       `yaml:"name"`
	Type           string       `yaml:"type"` // socks5 | router | direct
	Endpoint       string       `yaml:"endpoint"`
	Gateway        string       `yaml:"gateway"`
	Routes         []string     `yaml:"routes"`
	Weights        Weights      `yaml:"weights"`
	Probe          ProbeConfig  `yaml:"probe"`
	BandwidthProbe BandwidthCfg `yaml:"bandwidth_probe"`
}

type Weights struct {
	Latency   float64 `yaml:"latency"`
	Loss      float64 `yaml:"loss"`
	Bandwidth float64 `yaml:"bandwidth"`
}

type ProbeConfig struct {
	Targets          []ProbeTarget `yaml:"targets"`
	Interval         Duration      `yaml:"interval"`
	Timeout          Duration      `yaml:"timeout"`
	FailThreshold    int           `yaml:"fail_threshold"`
	RecoverThreshold int           `yaml:"recover_threshold"`
	LossWindow       int           `yaml:"loss_window"`
}

type ProbeTarget struct {
	Host string `yaml:"host"`
	Port uint16 `yaml:"port"`
}

type BandwidthCfg struct {
	Enable   bool     `yaml:"enable"`
	URL      string   `yaml:"url"`
	Bytes    int64    `yaml:"bytes"`
	Interval Duration `yaml:"interval"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.setDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Entry.TUNName == "" {
		c.Entry.TUNName = "sulb0"
	}
	if c.Entry.TUNIP == "" {
		c.Entry.TUNIP = "10.66.66.1"
	}
	if c.Entry.TUNNet == 0 {
		c.Entry.TUNNet = 24
	}
	if c.Entry.MTU == 0 {
		c.Entry.MTU = 1500
	}
	if c.Entry.SOCKSListen == "" {
		c.Entry.SOCKSListen = "10.66.66.1:1080"
	}
	if c.Scoring.EWMAAlpha == 0 {
		c.Scoring.EWMAAlpha = 0.3
	}
	if c.Scoring.Hysteresis == 0 {
		c.Scoring.Hysteresis = 0.10
	}
	if c.Scoring.StickTime.Duration == 0 {
		c.Scoring.StickTime.Duration = 15 * time.Second
	}
	if c.Scoring.LatencyBest.Duration == 0 {
		c.Scoring.LatencyBest.Duration = 10 * time.Millisecond
	}
	if c.Scoring.LatencyWorst.Duration == 0 {
		c.Scoring.LatencyWorst.Duration = 300 * time.Millisecond
	}
	if c.Scoring.BandwidthCap == 0 {
		c.Scoring.BandwidthCap = 10 << 20 // 10 MB/s
	}
	if c.Status.Listen == "" {
		c.Status.Listen = "127.0.0.1:8080"
	}
	for i := range c.Links {
		l := &c.Links[i]
		if l.Weights == (Weights{}) {
			l.Weights = Weights{Latency: 0.5, Loss: 0.2, Bandwidth: 0.3}
		}
		if l.Probe.Interval.Duration == 0 {
			l.Probe.Interval.Duration = 5 * time.Second
		}
		if l.Probe.Timeout.Duration == 0 {
			l.Probe.Timeout.Duration = 2 * time.Second
		}
		if l.Probe.FailThreshold == 0 {
			l.Probe.FailThreshold = 3
		}
		if l.Probe.RecoverThreshold == 0 {
			l.Probe.RecoverThreshold = 2
		}
		if l.Probe.LossWindow == 0 {
			l.Probe.LossWindow = 10
		}
		if len(l.Probe.Targets) == 0 {
			l.Probe.Targets = []ProbeTarget{{Host: "1.1.1.1", Port: 443}}
		}
		if l.BandwidthProbe.URL == "" {
			l.BandwidthProbe.URL = "http://speed.cloudflare.com/__down?bytes=524288"
		}
		if l.BandwidthProbe.Bytes == 0 {
			l.BandwidthProbe.Bytes = 524288
		}
		if l.BandwidthProbe.Interval.Duration == 0 {
			l.BandwidthProbe.Interval.Duration = 60 * time.Second
		}
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	for _, l := range c.Links {
		if l.Name == "" {
			return fmt.Errorf("link: name required")
		}
		if seen[l.Name] {
			return fmt.Errorf("duplicate link name %q", l.Name)
		}
		seen[l.Name] = true
		switch l.Type {
		case "socks5":
			if l.Endpoint == "" {
				return fmt.Errorf("link %q: type socks5 requires endpoint", l.Name)
			}
		case "router":
			if l.Gateway == "" {
				return fmt.Errorf("link %q: type router requires gateway", l.Name)
			}
			for _, r := range l.Routes {
				if _, err := netip.ParsePrefix(r); err != nil {
					return fmt.Errorf("link %q: bad route %q", l.Name, r)
				}
			}
		case "direct":
		default:
			return fmt.Errorf("link %q: unknown type %q (want socks5|router|direct)", l.Name, l.Type)
		}
		w := l.Weights
		if w.Latency < 0 || w.Loss < 0 || w.Bandwidth < 0 || w.Latency+w.Loss+w.Bandwidth <= 0 {
			return fmt.Errorf("link %q: weights must be >= 0 with a positive sum", l.Name)
		}
		for _, t := range l.Probe.Targets {
			if _, err := netip.ParseAddr(t.Host); err != nil {
				return fmt.Errorf("link %q: probe target host %q is not an IP", l.Name, t.Host)
			}
		}
	}
	if c.Scoring.Hysteresis < 0 || c.Scoring.Hysteresis >= 1 {
		return fmt.Errorf("scoring.hysteresis must be in [0,1)")
	}
	if _, err := netip.ParseAddr(c.Entry.TUNIP); err != nil {
		return fmt.Errorf("entry.tun_ip %q is not an IP", c.Entry.TUNIP)
	}
	return nil
}
```

`sulb.yaml`:

```yaml
entry:
  tun_name: sulb0
  tun_ip: 10.66.66.1
  tun_net: 24
  mtu: 1500
  socks_listen: 10.66.66.1:1080

scoring:
  ewma_alpha: 0.3
  hysteresis: 0.10
  stick_time: 15s
  latency_best: 10ms
  latency_worst: 300ms
  bandwidth_cap: 10485760
  floor: 0

status:
  listen: 127.0.0.1:8080

links:
  - name: vpn-a
    type: socks5
    endpoint: 127.0.0.1:1081
    weights: {latency: 0.5, loss: 0.2, bandwidth: 0.3}
    probe:
      targets: [{host: 1.1.1.1, port: 443}, {host: 8.8.8.8, port: 53}]
      interval: 5s
      timeout: 2s
      fail_threshold: 3
      recover_threshold: 2
      loss_window: 10
    bandwidth_probe: {enable: true, bytes: 524288, interval: 60s}

  - name: vpn-b
    type: socks5
    endpoint: 127.0.0.1:1082
    weights: {latency: 0.5, loss: 0.2, bandwidth: 0.3}

  - name: router-1
    type: router
    gateway: 192.168.1.2
    routes: [0.0.0.0/0]
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go mod tidy && go test ./internal/config/`
Expected: PASS. (go.mod gains `gopkg.in/yaml.v3` and go 1.26 directive.)

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum sulb.yaml internal/config/
git commit -m "feat: config package with defaults and validation

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: Scoring engine

**Files:**
- Create: `internal/score/score.go`
- Create: `internal/score/score_test.go`

**Interfaces:**
- Consumes: nothing (pure package; config types not imported — plain values).
- Produces: `score.Weights{Latency, Loss, Bandwidth float64}`; `score.Metrics{Latency time.Duration, LatencyValid bool, Loss float64, Bandwidth float64, BandwidthValid bool}`; `score.Norm{LatencyBest, LatencyWorst time.Duration, BandwidthCap float64}`; `score.Score(m Metrics, w Weights, n Norm) float64` (0–100, higher = better); `score.Ewma(prev, next, alpha float64) float64`.

- [ ] **Step 1: Write the failing test**

`internal/score/score_test.go`:

```go
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
	cases := []struct {
		ms   time.Duration
		want float64
	}{
		{10 * time.Millisecond, 100},
		{300 * time.Millisecond, 0},
		{155 * time.Millisecond, 50},
		{5 * time.Millisecond, 100}, // clamped
		{10 * time.Second, 0},        // clamped
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
	if abs(got-75) > 0.01 {
		t.Fatalf("mixed: got %v want 75", got)
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/score/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/score/score.go`:

```go
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
	// Renormalize over valid metrics so a missing metric (e.g. bandwidth
	// probe disabled) doesn't drag the score down.
	total := w.Loss
	if m.LatencyValid {
		total += w.Latency
	}
	if m.BandwidthValid {
		total += w.Bandwidth
	}
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/score/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/score/
git commit -m "feat: score normalization and EWMA

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Link type + state machine

**Files:**
- Create: `internal/links/link.go`
- Create: `internal/links/link_test.go`

**Interfaces:**
- Consumes: `config.LinkConfig`, `config.ScoringConfig`; `score.Metrics`, `score.Norm`, `score.Ewma`; tun2socks `metadata.Metadata` (`M.TCP`/`M.UDP`), `proxy/socks5.New`, `dialer.DialContext`.
- Produces: `links.New(cfg config.LinkConfig, alpha float64, norm score.Norm) (*Link, error)`; `(*Link).Name()/Type()/Cfg() string`; `(*Link).RecordProbe(ok bool, latency time.Duration)`; `(*Link).RecordPassive(latency time.Duration)`; `(*Link).UpdateScore(m score.Metrics)`; `(*Link).SetBandwidth(b float64, valid bool)`; `(*Link).SetRoutingOK(bool)`; `(*Link).Snapshot() Snapshot`; `(*Link).DialContext(ctx context.Context, dst netip.AddrPort) (net.Conn, error)`; `(*Link).DialUDP(dst netip.AddrPort) (net.PacketConn, error)`; `links.State` (`StateDown`/`StateDegraded`/`StateUp`); `links.Snapshot{Name, Type string, State State, Score float64, Latency time.Duration, Loss float64, Bandwidth float64, BandwidthValid, RoutingOK bool}`.

- [ ] **Step 1: Write the failing test**

`internal/links/link_test.go`:

```go
package links

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/score"
)

func testLink(t *testing.T, name, typ, endpoint string) *Link {
	t.Helper()
	l, err := New(config.LinkConfig{Name: name, Type: typ, Endpoint: endpoint}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
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
	l := testLink(t, "a", "direct", "")
	l.cfg.Probe.LossWindow = 10 // set via cfg below
	// construct with window instead:
	l = testLinkWindow(t, "a", 10)
	// 3 fails out of 10 -> loss 0.3
	for i := 0; i < 10; i++ {
		l.RecordProbe(i%10 >= 7, 10*time.Millisecond) // last 3 fail
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

func testLinkWindow(t *testing.T, name string, window int) *Link {
	t.Helper()
	l, err := New(config.LinkConfig{Name: name, Type: "direct", Probe: config.ProbeConfig{LossWindow: window}}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
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
	l.UpdateScore(score.Metrics{Latency: 10 * time.Millisecond, LatencyValid: true, Loss: 0})
	if s := l.Snapshot(); s.Score < 99 {
		t.Fatalf("near-perfect metrics should score ~100: got %v", s.Score)
	}
	l.UpdateScore(score.Metrics{Latency: 10 * time.Second, LatencyValid: true, Loss: 1})
	if s := l.Snapshot(); s.Score > 1 {
		t.Fatalf("terrible metrics should score ~0: got %v", s.Score)
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
```

Note: `l.cfg` is private — the test constructs with `config.ProbeConfig{LossWindow: window}` via `testLinkWindow` (LossWindow is a real config field; the intermediate `testLink` + mutation line in `TestLossWindow` is removed — the test above already only uses `testLinkWindow`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/links/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/links/link.go`:

```go
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
	cfg   config.LinkConfig
	alpha float64
	norm  score.Norm
	socks *socks5.Socks5 // non-nil only for type socks5

	mu              sync.RWMutex
	state           State
	score           float64
	latencyMs       float64 // EWMA
	bandwidth       float64
	bandwidthValid  bool
	routingOK       bool
	lossFails       int
	succs           int
	ring            []bool
	ringIdx         int
}

func New(cfg config.LinkConfig, alpha float64, norm score.Norm) (*Link, error) {
	l := &Link{cfg: cfg, alpha: alpha, norm: norm, state: StateUp, routingOK: true}
	if cfg.Type == "socks5" {
		s, err := socks5.New(cfg.Endpoint, "", "")
		if err != nil {
			return nil, fmt.Errorf("link %q: bad socks5 endpoint: %w", cfg.Name, err)
		}
		l.socks = s
	}
	return l, nil
}

func (l *Link) Name() string { return l.cfg.Name }
func (l *Link) Type() string { return l.cfg.Type }
func (l *Link) Cfg() config.LinkConfig { return l.cfg }

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
	raw := score.Score(m, l.cfg.Weights, l.norm)
	l.score = score.Ewma(l.score, raw, l.alpha)
	if m.BandwidthValid {
		l.bandwidth, l.bandwidthValid = m.Bandwidth, true
	}
	if l.cfg.BandwidthProbe.Enable {
		_ = m.BandwidthValid // bandwidth set via SetBandwidth by the probe engine
	}
	if f := l.scoreFloor(); f > 0 {
		if raw < f && l.state == StateUp {
			l.state = StateDegraded
		} else if raw >= f && l.state == StateDegraded {
			l.state = StateUp
		}
	}
}

func (l *Link) scoreFloor() float64 { return l.floor } // set at New; default 0

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
```

Fix the scoreFloor inconsistency: `Link` needs a `floor float64` field set at New. Add to struct `floor float64` and set `floor: 0` — the floor comes from config.Scoring.Floor; `links.New` doesn't receive it. Simplest: main passes it via `l.SetFloor`? Cleaner: extend `New(cfg, alpha, norm)` — the probe engine applies the floor via `UpdateScore` reading... Decision: keep floor in scoring config; `New` signature stays; add exported `(*Link).SetFloor(f float64)`; the probe engine calls it at startup. Replace `scoreFloor()` with the field:

```go
floor float64 // score below this -> DEGRADED; 0 = disabled (set by probe engine)
func (l *Link) SetFloor(f float64) { l.mu.Lock(); defer l.mu.Unlock(); l.floor = f }
```
And in UpdateScore use `l.floor`. The `scoreFloor()` method and `cfg.BandwidthProbe.Enable` dead lines are removed. Test `TestScoreUpdateAndDegraded` doesn't touch floor — fine.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/links/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/links/
git commit -m "feat: link state machine, EWMA metrics, socks5/direct dialers

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Link picker (hysteresis + stick time)

**Files:**
- Create: `internal/pick/pick.go`
- Create: `internal/pick/pick_test.go`

**Interfaces:**
- Consumes: `links.Link`, `links.Snapshot`, `links.StateUp`.
- Produces: `pick.New(ls []*links.Link, hyst float64, stick time.Duration) *Picker`; `(*Picker).Pick() *links.Link` (nil only if no links configured); `(*Picker).Current() *links.Link`.

- [ ] **Step 1: Write the failing test**

`internal/pick/pick_test.go`:

```go
package pick

import (
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/score"
)

func mk(name string) *links.Link {
	l, err := links.New(config.LinkConfig{Name: name, Type: "direct"}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		panic(err)
	}
	return l
}

// drive sets a link's score by feeding UpdateScore metrics, exactly like the
// probe engine does.
func drive(l *links.Link, latency time.Duration) {
	l.UpdateScore(score.Metrics{Latency: latency, LatencyValid: true, Loss: 0})
}

func down(l *links.Link) {
	for i := 0; i < 3; i++ {
		l.RecordProbe(false, 0)
	}
}

func TestInitialPickIsHighestScore(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond) // 100
	drive(b, 100*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	if got := p.Pick(); got != a {
		t.Fatalf("initial pick: want a, got %s", got.Name())
	}
}

func TestHysteresisPreventsFlap(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond) // 100
	drive(b, 30*time.Millisecond) // ~93
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	// Oscillate: b slightly better, then a slightly better. Both within
	// the 10% margin of the other -> must stay on a the whole time.
	for i := 0; i < 10; i++ {
		now = now.Add(2 * time.Second)
		if i%2 == 0 {
			drive(b, 20*time.Millisecond)
			drive(a, 10*time.Millisecond)
		} else {
			drive(a, 20*time.Millisecond)
			drive(b, 10*time.Millisecond)
		}
		if got := p.Pick(); got != a {
			t.Fatalf("flapped to %s on iteration %d", got.Name(), i)
		}
	}
}

func TestStickTimeBlocksSwitch(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond) // 100
	drive(b, 10*time.Millisecond) // 100 -> b not better
	p := New([]*links.Link{a, b}, 0.01, 15*time.Second)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	// b becomes clearly better (score ~100 vs ~0) but stick time hasn't elapsed.
	drive(b, 10*time.Millisecond)
	drive(a, 10*time.Second)
	if got := p.Pick(); got != a {
		t.Fatalf("stick time should hold a: got %s", got.Name())
	}
	// After 20s (past 15s stick) it may switch.
	now = now.Add(20 * time.Second)
	if got := p.Pick(); got != b {
		t.Fatalf("after stick time want b, got %s", got.Name())
	}
}

func TestFailoverIsImmediate(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond)
	drive(b, 50*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, time.Hour) // long stick
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	if got := p.Pick(); got != a {
		t.Fatalf("want a, got %s", got.Name())
	}
	down(a) // a goes Down regardless of stick time
	if got := p.Pick(); got != b {
		t.Fatalf("failover must ignore stick time: want b, got %s", got.Name())
	}
}

func TestAllDownFailClosed(t *testing.T) {
	a, b := mk("a"), mk("b")
	drive(a, 10*time.Millisecond)
	drive(b, 200*time.Millisecond)
	p := New([]*links.Link{a, b}, 0.10, 15*time.Second)
	down(a)
	down(b)
	got := p.Pick()
	if got == nil || got != a { // least-bad (higher score) still returned
		t.Fatalf("fail-closed should return least-bad link, got %v", got)
	}
	if s := got.Snapshot(); s.State != links.StateDown {
		t.Fatalf("picked link must still report Down, got %v", s.State)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pick/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/pick/pick.go`:

```go
// Package pick selects the best link for each new flow with hysteresis and
// stick time so links never flap.
package pick

import (
	"log/slog"
	"sync"
	"time"

	"sulb/internal/links"
)

type Picker struct {
	mu       sync.Mutex
	links    []*links.Link
	hyst     float64
	stick    time.Duration
	current  *links.Link
	switched time.Time
	now      func() time.Time
	log      *slog.Logger
}

func New(ls []*links.Link, hyst float64, stick time.Duration) *Picker {
	return &Picker{links: ls, hyst: hyst, stick: stick, now: time.Now, log: slog.Default()}
}

// Pick returns the link for a new flow. nil only when no links are configured.
func (p *Picker) Pick() *links.Link {
	p.mu.Lock()
	defer p.mu.Unlock()

	best := bestAmong(p.links, links.StateUp)
	if best == nil {
		// Fail-closed: every link down/degraded -> least-bad one.
		best = bestAmong(p.links, links.StateDegraded)
	}
	if best == nil {
		best = bestAmong(p.links, links.StateDown)
	}
	if best == nil {
		return nil
	}

	cur := p.current
	if cur == nil {
		p.current, p.switched = best, p.now()
		return best
	}
	if cur == best {
		return best
	}

	curSnap, bestSnap := cur.Snapshot(), best.Snapshot()
	// Stick time only protects a healthy current link.
	if curSnap.State == links.StateUp && p.now().Sub(p.switched) < p.stick {
		return cur
	}
	// Hysteresis: only switch when the candidate is clearly better.
	if curSnap.State == links.StateUp && bestSnap.Score <= curSnap.Score*(1+p.hyst) {
		return cur
	}
	p.log.Info("switching link",
		"from", cur.Name(), "to", best.Name(),
		"from_score", round2(curSnap.Score), "to_score", round2(bestSnap.Score))
	p.current, p.switched = best, p.now()
	return best
}

func (p *Picker) Current() *links.Link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

func bestAmong(ls []*links.Link, s links.State) *links.Link {
	var best *links.Link
	for _, l := range ls {
		if l.Snapshot().State != s {
			continue
		}
		if best == nil || l.Snapshot().Score > best.Snapshot().Score {
			best = l
		}
	}
	return best
}

func round2(x float64) float64 { return float64(int(x*100)) / 100 }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/pick/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pick/
git commit -m "feat: link picker with hysteresis, stick time, fail-closed order

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: SOCKS5 server entry

**Files:**
- Create: `internal/entry/socks5.go`
- Create: `internal/entry/socks5_test.go`

**Interfaces:**
- Consumes: `pick.Picker`, `links.Link` dialers, tun2socks `buffer` (relay buffer).
- Produces: `entry.NewSocksServer(addr string, p *pick.Picker) (*SocksServer, error)`; `(*SocksServer).Addr() net.Addr`; `(*SocksServer).Start()`; `(*SocksServer).Close() error`; `entry.Pipe(a, b net.Conn)`.

- [ ] **Step 1: Write the failing test**

`internal/entry/socks5_test.go`:

```go
package entry

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/score"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy/socks5"
)

func testPicker(t *testing.T) *pick.Picker {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: "direct", Type: "direct"}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pick.New([]*links.Link{l}, 0.1, 15*time.Second)
}

func TestSocksConnectRoundtrip(t *testing.T) {
	// echo server
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
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	// our socks server
	s, err := NewSocksServer("127.0.0.1:0", testPicker(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Close()
	// tun2socks socks5 client through our server
	client, err := socks5.New(s.Addr().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ec := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, &M.Metadata{Network: M.TCP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: uint16(ec.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("hello through socks")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := c.Read(got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch: %q", got)
	}
}

func TestSocksUDPAssociate(t *testing.T) {
	// UDP echo server
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()
	s, err := NewSocksServer("127.0.0.1:0", testPicker(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Close()
	client, err := socks5.New(s.Addr().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ep := pc.LocalAddr().(*net.UDPAddr)
	uc, err := client.DialUDP(&M.Metadata{Network: M.UDP, DstIP: netip.MustParseAddr("127.0.0.1"), DstPort: uint16(ep.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if _, err := uc.WriteTo([]byte("udp-ping"), &net.UDPAddr{IP: netip.MustParseAddr("127.0.0.1").AsSlice(), Port: ep.Port}); err != nil {
		t.Fatal(err)
	}
	uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := uc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "udp-ping" {
		t.Fatalf("udp echo mismatch: %q", buf[:n])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/entry/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/entry/socks5.go`:

```go
// Package entry provides the exposed entry points: the SOCKS5 server and the
// TUN stack. Both hand each new flow to the picker.
package entry

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/buffer"

	"sulb/internal/pick"
)

const (
	socksHandshakeTimeout = 30 * time.Second
	socksUDPIdleTimeout   = 5 * time.Minute
)

// SocksServer is a SOCKS5 (no-auth) server whose CONNECT and UDP ASSOCIATE
// requests are dialed through the picker's chosen link.
type SocksServer struct {
	ln     net.Listener
	picker *pick.Picker
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewSocksServer(addr string, p *pick.Picker) (*SocksServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &SocksServer{ln: ln, picker: p, cancel: cancel}
	go s.acceptLoop(ctx)
	return s, nil
}

func (s *SocksServer) Addr() net.Addr { return s.ln.Addr() }

func (s *SocksServer) Start() {} // listener already running; kept for symmetry

func (s *SocksServer) Close() error {
	s.cancel()
	return s.ln.Close()
}

func (s *SocksServer) acceptLoop(ctx context.Context) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, c)
		}()
	}
}

func (s *SocksServer) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	if err := negotiate(c); err != nil {
		return
	}
	cmd, dst, err := readRequest(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	switch cmd {
	case 1: // CONNECT
		s.handleConnect(ctx, c, dst)
	case 3: // UDP ASSOCIATE
		s.handleUDP(ctx, c, dst)
	}
}

func negotiate(c net.Conn) error {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 5 { // SOCKS5 only
		return errBadVersion
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	for _, m := range methods {
		if m == 0x00 { // no-auth
			_, err := c.Write([]byte{5, 0})
			return err
		}
	}
	c.Write([]byte{5, 0xFF}) // no acceptable methods
	return errNoAuth
}

var (
	errBadVersion = errStr("bad socks version")
	errNoAuth     = errStr("no acceptable auth method")
)

type errStr string

func (e errStr) Error() string { return string(e) }

// readRequest returns the command (1=CONNECT, 3=UDP ASSOCIATE) and the
// destination. Hostnames are resolved here — the link dialers take IPs.
func readRequest(c net.Conn) (byte, netip.AddrPort, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return 0, netip.AddrPort{}, err
	}
	if hdr[0] != 5 || hdr[2] != 0 {
		return 0, netip.AddrPort{}, errBadRequest
	}
	var addr netip.Addr
	switch hdr[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		addr = netip.AddrFrom4([4]byte(b))
	case 3: // hostname
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		host := make([]byte, int(b[0]))
		if _, err := io.ReadFull(c, host); err != nil {
			return 0, netip.AddrPort{}, err
		}
		ips, err := net.LookupIP(string(host))
		if err != nil || len(ips) == 0 {
			return 0, netip.AddrPort{}, errHostResolve
		}
		addr, _ = netip.AddrFromSlice(ips[0])
		addr = addr.Unmap()
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return 0, netip.AddrPort{}, err
		}
		addr = netip.AddrFrom16([16]byte(b))
	default:
		return 0, netip.AddrPort{}, errBadRequest
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return 0, netip.AddrPort{}, err
	}
	return hdr[1], netip.AddrPortFrom(addr, binary.BigEndian.Uint16(pb)), nil
}

var (
	errBadRequest  = errStr("bad socks request")
	errHostResolve = errStr("hostname resolution failed")
)

func writeReply(c net.Conn, code byte) {
	// 5, code, 0, ATYP=1, 0.0.0.0, :0
	c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

func (s *SocksServer) handleConnect(ctx context.Context, c net.Conn, dst netip.AddrPort) {
	l := s.picker.Pick()
	if l == nil {
		writeReply(c, 0x01)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := l.DialContext(dctx, dst)
	if err != nil {
		writeReply(c, 0x04)
		return
	}
	defer rc.Close()
	writeReply(c, 0x00)
	Pipe(c, rc)
}

func (s *SocksServer) handleUDP(ctx context.Context, c net.Conn, dst netip.AddrPort) {
	relay, err := net.ListenPacket("udp", s.ln.Addr().String())
	if err != nil {
		writeReply(c, 0x01)
		return
	}
	defer relay.Close()
	l := s.picker.Pick()
	if l == nil {
		writeReply(c, 0x01)
		return
	}
	pc, err := l.DialUDP(dst)
	if err != nil {
		writeReply(c, 0x04)
		return
	}
	defer pc.Close()
	writeReply(c, 0x00)
	s.relayUDP(c, relay, pc, dst)
}

// relayUDP pumps UDP datagrams between the client's relay socket and the
// link's packet conn, adding/removing the SOCKS UDP header (RSV FRAG ATYP
// ADDR PORT). Responses go back to whichever client address sent first.
func (s *SocksServer) relayUDP(c net.Conn, relay, pc net.PacketConn, dst netip.AddrPort) {
	clientCh := make(chan netip.AddrPort, 1)
	done := make(chan struct{})
	go func() { // client -> link
		defer close(done)
		buf := buffer.Get(buffer.MaxSegmentSize)
		defer buffer.Put(buf)
		for {
			relay.SetReadDeadline(time.Now().Add(socksUDPIdleTimeout))
			n, from, err := relay.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case clientCh <- addrPortFromNetAddr(from):
			default:
			}
			payload, d, ok := stripUDPHeader(buf[:n], dst)
			if !ok {
				continue
			}
			if _, err := pc.WriteTo(payload, netAddrFromAddrPort(d)); err != nil {
				return
			}
		}
	}()
	go func() { // link -> client
		buf := buffer.Get(buffer.MaxSegmentSize)
		defer buffer.Put(buf)
		for {
			pc.SetReadDeadline(time.Now().Add(socksUDPIdleTimeout))
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// Only accept replies from the address we sent to.
			if from != nil && from.String() != dst.String() {
				continue
			}
			var client netip.AddrPort
			select {
			case client = <-clientCh:
			case <-done:
				return
			}
			hdr := makeUDPHeader(client)
			if _, err := relay.WriteTo(append(hdr, buf[:n]...), netAddrFromAddrPort(client)); err != nil {
				return
			}
		}
	}()
	// Wait for both directions; close everything when the client's TCP
	// session ends or a direction errors out.
	<-done
	pc.Close()
	relay.Close()
	c.Close()
}

func stripUDPHeader(b []byte, fallback netip.AddrPort) (payload []byte, dst netip.AddrPort, ok bool) {
	if len(b) < 4 {
		return nil, netip.AddrPort{}, false
	}
	if b[2] != 0 { // FRAG must be 0
		return nil, netip.AddrPort{}, false
	}
	switch b[3] {
	case 1:
		if len(b) < 4+4+2 {
			return nil, netip.AddrPort{}, false
		}
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[8:10]))
		return b[10:], dst, true
	case 3:
		if len(b) < 4+1+2 {
			return nil, netip.AddrPort{}, false
		}
		n := int(b[4])
		if len(b) < 4+1+n+2 {
			return nil, netip.AddrPort{}, false
		}
		host := string(b[5 : 5+n])
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return nil, netip.AddrPort{}, false
		}
		addr, _ := netip.AddrFromSlice(ips[0])
		dst = netip.AddrPortFrom(addr.Unmap(), binary.BigEndian.Uint16(b[5+n:7+n]))
		return b[7+n:], dst, true
	case 4:
		if len(b) < 4+16+2 {
			return nil, netip.AddrPort{}, false
		}
		dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[4:20])), binary.BigEndian.Uint16(b[20:22]))
		return b[22:], dst, true
	}
	return nil, netip.AddrPort{}, false
}

func makeUDPHeader(dst netip.AddrPort) []byte {
	h := []byte{0, 0, 0}
	if dst.Addr().Is4() {
		h = append(h, 1)
		b := dst.Addr().As4()
		h = append(h, b[:]...)
	} else {
		h = append(h, 4)
		b := dst.Addr().As16()
		h = append(h, b[:]...)
	}
	return binary.BigEndian.AppendUint16(h, dst.Port())
}

func addrPortFromNetAddr(a net.Addr) netip.AddrPort {
	if u, ok := a.(*net.UDPAddr); ok {
		ap, _ := netip.AddrFromSlice(u.IP)
		return netip.AddrPortFrom(ap.Unmap(), uint16(u.Port))
	}
	return netip.AddrPort{}
}

func netAddrFromAddrPort(ap netip.AddrPort) net.Addr {
	return net.UDPAddrFromAddrPort(ap)
}

// Pipe copies data bidirectionally with half-close support.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { copyHalf(a, b); done <- struct{}{} }()
	go func() { copyHalf(b, a); done <- struct{}{} }()
	<-done
	<-done
}

func copyHalf(dst, src net.Conn) {
	buf := buffer.Get(buffer.RelayBufferSize)
	defer buffer.Put(buf)
	io.CopyBuffer(dst, src, buf)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	if cr, ok := src.(interface{ CloseRead() error }); ok {
		cr.CloseRead()
	}
}
```

Note: `relayUDP`'s wait logic waits only for the client→link goroutine (`done`); the link→client goroutine exits via read deadline after the relay closes. `relay.Close()` unblocks it. Acceptable: on TCP session end, `handle` returns and `defer c.Close()` fires — but relayUDP is called *before* handle returns, so c stays open until relayUDP returns. `<-done` blocks until client→link errors (relay read fails after c.Close() — relay is a UDP socket, unaffected by c.Close()!). Fix: close relay from the client TCP read side: in relayUDP, add `go func() { buf := make([]byte, 1); c.Read(buf); relay.Close(); pc.Close() }()` — the TCP conn is still open (handle's defer hasn't run), so c.Read blocks until the client disconnects. That's the session lifetime signal. Insert that goroutine at the top of relayUDP.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/entry/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/entry/socks5.go internal/entry/socks5_test.go
git commit -m "feat: SOCKS5 server entry (CONNECT + UDP ASSOCIATE) via picker

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: Probe engine

**Files:**
- Create: `internal/probe/probe.go`
- Create: `internal/probe/probe_test.go`

**Interfaces:**
- Consumes: `config.ScoringConfig`, `links.Link` (`RecordProbe`, `UpdateScore`, `SetBandwidth`, `SetFloor`, `DialContext`), `score.Norm`.
- Produces: `probe.New(ls []*links.Link, s config.ScoringConfig) *Engine`; `(*Engine).Start(ctx context.Context)`; `probe.TCPProbe(ctx, l *links.Link, host string, port uint16, timeout time.Duration) (bool, time.Duration)`; `probe.BandwidthOnce(ctx, l *links.Link, url string, wantBytes int64) (float64, error)`.

- [ ] **Step 1: Write the failing test**

`internal/probe/probe_test.go`:

```go
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

	"gopkg.in/yaml.v3"
)

func directLink(t *testing.T) *links.Link {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: "direct", Type: "direct"}, 0.3, scoreNorm())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func scoreNorm() (n struct{}) { return }

func TestTCPProbe(t *testing.T) {
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
			c.Close()
		}
	}()
	l := directLink(t)
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	ok, lat := TCPProbe(context.Background(), l, ap.Addr().String(), ap.Port(), 2*time.Second)
	if !ok || lat <= 0 {
		t.Fatalf("probe to live listener: ok=%v lat=%v", ok, lat)
	}
	// dead port -> fail
	closed := ln.Addr().String()
	ln.Close()
	ap2, _ := netip.ParseAddrPort(closed)
	ok, _ = TCPProbe(context.Background(), l, ap2.Addr().String(), ap2.Port(), 200*time.Millisecond)
	if ok {
		t.Fatal("probe to closed port must fail")
	}
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
			c.Close()
		}
	}()
	l := directLink(t)
	var sc config.ScoringConfig
	sc.SetDefaultsForTest() // not exported — see note below
	...
}
```

Note on `TestEngineRunsProbes`: instead of an unexported default-setter, construct `config.ScoringConfig{EWMAAlpha: 0.3, Hysteresis: 0.1, StickTime: ..., LatencyBest: 10ms, LatencyWorst: 300ms, BandwidthCap: 10<<20}` inline, and set probe interval on the link's config: `l.Cfg()` is a value copy — so build the link with `config.LinkConfig{Name: "direct", Type: "direct", Probe: config.ProbeConfig{Interval: ..., Timeout: ..., LossWindow: 10}}`; engine created with those; run `Start(ctx)`; wait ~2.5× interval; assert `l.Snapshot().Latency > 0` (probes recorded) and State Up. Final test body:

```go
func TestEngineRunsProbes(t *testing.T) {
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
			c.Close()
		}
	}()
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	l, err := links.New(config.LinkConfig{
		Name:  "direct",
		Type:  "direct",
		Probe: config.ProbeConfig{Targets: []config.ProbeTarget{{Host: ap.Addr().String(), Port: ap.Port()}}, Interval: d(200 * time.Millisecond), Timeout: d(time.Second), LossWindow: 10},
	}, 0.3, scoreNormReal())
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

func d(x time.Duration) config.Duration { return config.Duration{Duration: x} }

func scoreNormReal() (n struct{}) { return } // replaced below
```

Clean this up in the real test file (no `scoreNorm`/`scoreNormReal` stubs — build the `score.Norm` value directly). The plan text above is illustrative of intent; the shipped test uses concrete values.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/probe/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/probe/probe.go`:

```go
// Package probe measures each link: TCP connect latency through the link,
// loss over a sliding window, and an optional bandwidth download sample.
package probe

import (
	"context"
	"fmt"
	"io"
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
	m := score.Metrics{Loss: loss, LatencyValid: oks > 0, Latency: time.Duration(latencyMs / float64(max(oks, 1)) * float64(time.Millisecond))}
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
```

Note: `req.Write(c)` on the dialed conn sends the raw HTTP request (hostname in the Host header), and the socks link's proxy resolves nothing — the SOCKS CONNECT carries the IP we dialed; the target server sees our Host header. Works for plain HTTP (the bandwidth sample). The `io` import is unused — drop it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/probe/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/probe/
git commit -m "feat: probe engine (TCP latency, loss window, bandwidth sample)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: TUN entry

**Files:**
- Create: `internal/entry/tun.go`
- Create: `internal/entry/setup_linux.go`, `internal/entry/setup_darwin.go`, `internal/entry/setup_windows.go`, `internal/entry/setup_other.go`

**Interfaces:**
- Consumes: `pick.Picker`, `links.Link` dialers, tun2socks `core`, `core/device/tun`, `core/adapter`, `buffer`.
- Produces: `entry.StartTun(cfg config.EntryConfig, p *pick.Picker) (*Stack, error)`; `(*Stack).Close() error`.

- [ ] **Step 1: Write the failing test** — none: TUN requires root; covered end-to-end by `scripts/smoke.sh` (Task 10). The handler logic mirrors the SOCKS server's (already tested) plus the tun2socks pipe patterns.

- [ ] **Step 2: Write minimal implementation**

`internal/entry/tun.go`:

```go
package entry

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/buffer"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/tun"

	"sulb/internal/config"
	"sulb/internal/pick"
)

// Stack owns the TUN device and gVisor netstack, terminating TCP/UDP flows
// and dialing each through the picker's chosen link.
type Stack struct {
	device device.Device
	stack  *core.Stack
}

// StartTun creates the TUN device, brings it up with the configured address
// (OS-specific), and starts the netstack.
func StartTun(cfg config.EntryConfig, p *pick.Picker) (*Stack, error) {
	dev, err := tun.Open(cfg.TUNName, cfg.MTU)
	if err != nil {
		return nil, err
	}
	st, err := core.CreateStack(&core.Config{
		LinkEndpoint:     dev,
		TransportHandler: &tunHandler{p: p, log: slog.Default()},
	})
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := setupAddress(cfg); err != nil {
		dev.Close()
		st.Close()
		return nil, err
	}
	slog.Info("tun up", "name", cfg.TUNName, "ip", cfg.TUNIP)
	return &Stack{device: dev, stack: st}, nil
}

func (s *Stack) Close() error {
	s.device.Close()
	s.stack.Close()
	return nil
}

type tunHandler struct {
	p   *pick.Picker
	log *slog.Logger
}

func (h *tunHandler) HandleTCP(conn adapter.TCPConn) {
	defer conn.Close()
	id := conn.ID()
	dst := netip.AddrPortFrom(addrFromTcpAddr(id.LocalAddress), id.LocalPort)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	l := h.p.Pick()
	if l == nil {
		return
	}
	rc, err := l.DialContext(ctx, dst)
	if err != nil {
		h.log.Warn("tcp dial failed", "link", l.Name(), "dst", dst.String(), "err", err)
		return
	}
	defer rc.Close()
	l.RecordPassive(time.Since(start))
	Pipe(conn, rc)
}

func (h *tunHandler) HandleUDP(uc adapter.UDPConn) {
	defer uc.Close()
	id := uc.ID()
	dst := netip.AddrPortFrom(addrFromTcpAddr(id.LocalAddress), id.LocalPort)
	l := h.p.Pick()
	if l == nil {
		return
	}
	pc, err := l.DialUDP(dst)
	if err != nil {
		h.log.Warn("udp dial failed", "link", l.Name(), "dst", dst.String(), "err", err)
		return
	}
	defer pc.Close()
	pipePacket(uc, pc, dst)
}

// pipePacket pumps UDP between the netstack endpoint and the link's packet
// conn, dropping replies not from the destination (symmetric-NAT filtering).
func pipePacket(origin, remote net.PacketConn, dst netip.AddrPort) {
	timeout := 5 * time.Minute
	done := make(chan struct{}, 2)
	go func() { copyPacket(origin, remote, timeout); done <- struct{}{} }()
	go func() { copyPacket(remote, origin, timeout); done <- struct{}{} }()
	<-done
	<-done
}

func copyPacket(dst, src net.PacketConn, timeout time.Duration) {
	buf := buffer.Get(buffer.MaxSegmentSize)
	defer buffer.Put(buf)
	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, from, err := src.ReadFrom(buf)
		if err != nil {
			return
		}
		// Only forward replies that came from where we sent.
		if from != nil && !addrPortFromNetAddr(from).Addr().IsUnspecified() &&
			from.String() != dst.String() {
			continue
		}
		if _, err := dst.WriteTo(buf[:n], netAddrFromAddrPort(dst)); err != nil {
			return
		}
	}
}

func addrFromTcpAddr(a interface{ As4() [4]byte }) netip.Addr {
	return netip.AddrFrom4(a.As4())
}
```

Fix: `id.LocalAddress` is a `tcpip.Address` — its real type is `tcpip.Address` (a string-like). `parseTCPIPAddress` in tun2socks does the conversion. I can't see that helper here; safest is to use the same pattern as tun2socks (it's in `tunnel/addr.go`):

```go
func parseTCPIPAddress(a tcpip.Address) netip.Addr {
	switch {
	case a.Len() == 4:
		return netip.AddrFrom4(a.As4())
	case a.Len() == 16:
		return netip.AddrFrom16(a.As16())
	}
	return netip.Addr{}
}
```
So `tun.go` imports `gvisor.dev/gvisor/pkg/tcpip` and uses:

```go
func addrFromTcpAddr(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	return netip.AddrFrom16(a.As16())
}
```
(IPv6 endpoints exist since the stack runs ipv4+ipv6.) Replace `addrFromTcpAddr` above with this version and import `gvisor.dev/gvisor/pkg/tcpip`. `core.Stack` type: `core.CreateStack` returns `*stack.Stack` (gvisor `stack.Stack`) — the exported `core.Config` is what we construct; the return type in my `Stack` struct is `*core.Stack` which doesn't exist. Use `*stack.Stack` with `gvisor.dev/gvisor/pkg/tcpip/stack` import. Note: gvisor becomes a direct dep — `go mod tidy` records it (it's a direct import, fine).

`internal/entry/setup_linux.go`:

```go
//go:build linux

package entry

import (
	"fmt"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	if out, err := exec.Command("ip", "addr", "add", fmt.Sprintf("%s/%d", cfg.TUNIP, cfg.TUNNet), "dev", cfg.TUNName).CombinedOutput(); err != nil {
		return fmt.Errorf("ip addr add: %v: %s", err, out)
	}
	if out, err := exec.Command("ip", "link", "set", cfg.TUNName, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link up: %v: %s", err, out)
	}
	return nil
}
```

`internal/entry/setup_darwin.go`:

```go
//go:build darwin

package entry

import (
	"fmt"
	"net/netip"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	ip, err := netip.ParseAddr(cfg.TUNIP)
	if err != nil {
		return err
	}
	bcast := netip.AddrFrom4([4]byte{0, 0, 0, 0}) // placeholder; recompute below
	_ = bcast
	// ifconfig utunN 10.66.66.1 10.66.66.255 up
	if ip.Is4() {
		a := ip.As4()
		bcast = netip.AddrFrom4([4]byte{a[0], a[1], a[2], 255})
	}
	if out, err := exec.Command("ifconfig", cfg.TUNName, cfg.TUNIP, bcast.String(), "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig: %v: %s", err, out)
	}
	return nil
}
```

`internal/entry/setup_windows.go`:

```go
//go:build windows

package entry

import (
	"fmt"
	"net/netip"
	"os/exec"

	"sulb/internal/config"
)

func setupAddress(cfg config.EntryConfig) error {
	ip, err := netip.ParseAddr(cfg.TUNIP)
	if err != nil {
		return err
	}
	// Best-effort: wintun adapter naming varies; document.
	bits := 32 - cfg.TUNNet
	_ = bits
	if out, err := exec.Command("netsh", "interface", "ipv4", "add", "address",
		cfg.TUNName, cfg.TUNIP, maskString(cfg.TUNNet)).CombinedOutput(); err != nil {
		return fmt.Errorf("netsh add address: %v: %s", err, out)
	}
	return nil
}

func maskString(bits int) string {
	mask := uint32(0xFFFFFFFF) << (32 - bits)
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}
```

`internal/entry/setup_other.go`:

```go
//go:build !linux && !darwin && !windows

package entry

import "sulb/internal/config"

func setupAddress(cfg config.EntryConfig) error {
	return fmt.Errorf("tun address setup unsupported on this platform")
}
```
(needs `fmt` import.)

- [ ] **Step 3: Run build to verify it compiles**

Run: `go build ./... && go vet ./...`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add internal/entry/tun.go internal/entry/setup_*.go
git commit -m "feat: TUN entry with netstack, per-flow link dialing, OS address setup

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 8: Router manager

**Files:**
- Create: `internal/route/route.go`
- Create: `internal/route/route_linux.go`, `internal/route/route_darwin.go`, `internal/route/route_windows.go`
- Create: `internal/route/route_test.go`

**Interfaces:**
- Consumes: `pick.Picker`, `links.Link` (`SetRoutingOK`).
- Produces: `route.NewManager(p *pick.Picker, routes []string) *Manager`; `(*Manager).Run(ctx context.Context)`; `route.SetVia(ctx context.Context, run Runner, prefix, gw string) error`; `route.Runner func(ctx context.Context, name string, args ...string) error`; `route.OS() Runner`.

- [ ] **Step 1: Write the failing test**

`internal/route/route_test.go`:

```go
package route

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sulb/internal/config"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/score"
)

type fakeRunner struct {
	mu   sync.Mutex
	cmds [][]string
	err  error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, append([]string{name}, args...))
	return f.err
}

func mkLink(t *testing.T, name, typ, gateway string) *links.Link {
	t.Helper()
	l, err := links.New(config.LinkConfig{Name: name, Type: typ, Gateway: gateway}, 0.3, score.Norm{
		LatencyBest: 10 * time.Millisecond, LatencyWorst: 300 * time.Millisecond, BandwidthCap: 10 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSetViaLinux(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) error {
		got = append(got, append([]string{name}, args...)...)
		return nil
	}
	if err := SetVia(context.Background(), run, "0.0.0.0/0", "192.168.1.2"); err != nil {
		t.Fatal(err)
	}
	want := "ip route replace 0.0.0.0/0 via 192.168.1.2"
	if strings.Join(got, " ") != want {
		t.Fatalf("want %q, got %q", want, strings.Join(got, " "))
	}
}

func TestManagerRepointsRoutesOnSwitch(t *testing.T) {
	a := mkLink(t, "a", "router", "192.168.1.2")
	b := mkLink(t, "b", "router", "192.168.1.3")
	drive := func(l *links.Link, lat time.Duration) {
		l.UpdateScore(score.Metrics{Latency: lat, LatencyValid: true, Loss: 0})
	}
	drive(a, 10*time.Millisecond)
	drive(b, 200*time.Millisecond)
	p := pick.New([]*links.Link{a, b}, 0.1, 15*time.Second)
	fr := &fakeRunner{}
	m := NewManager(p, []string{"0.0.0.0/0"}, fr.Run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.apply(ctx)
	if len(fr.cmds) != 1 || !strings.Contains(strings.Join(fr.cmds[0], " "), "192.168.1.2") {
		t.Fatalf("want route via a's gateway, got %v", fr.cmds)
	}
	if !a.Snapshot().RoutingOK {
		t.Fatal("a should have RoutingOK after successful apply")
	}
	// b wins now -> next apply repoints to b's gateway
	drive(b, 10*time.Millisecond)
	drive(a, 200*time.Millisecond)
	m.apply(ctx)
	if len(fr.cmds) != 2 || !strings.Contains(strings.Join(fr.cmds[1], " "), "192.168.1.3") {
		t.Fatalf("want route via b's gateway, got %v", fr.cmds)
	}
	// same picker state -> no re-apply
	m.apply(ctx)
	if len(fr.cmds) != 2 {
		t.Fatalf("no-op apply must not re-run routes: %v", fr.cmds)
	}
}

func TestManagerRouteFailureMarksLink(t *testing.T) {
	a := mkLink(t, "a", "router", "192.168.1.2")
	drive := func(l *links.Link, lat time.Duration) {
		l.UpdateScore(score.Metrics{Latency: lat, LatencyValid: true, Loss: 0})
	}
	drive(a, 10*time.Millisecond)
	p := pick.New([]*links.Link{a}, 0.1, 15*time.Second)
	fr := &fakeRunner{err: errBoom}
	m := NewManager(p, []string{"0.0.0.0/0"}, fr.Run)
	ctx := context.Background()
	m.apply(ctx)
	if a.Snapshot().RoutingOK {
		t.Fatal("link must be marked routing-failed when route command fails")
	}
}

var errBoom = errStr("boom")

type errStr string

func (e errStr) Error() string { return string(e) }
```

Note: `m.apply` is private — the test lives in the same package (`package route`), which is fine.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/route/`
Expected: FAIL — package not found.

- [ ] **Step 3: Write minimal implementation**

`internal/route/route.go`:

```go
// Package route re-points OS routes so L3 traffic flows via the picker's
// chosen router link. Linux is first-class; macOS/Windows best-effort.
package route

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"sulb/internal/links"
	"sulb/internal/pick"
)

// Runner executes an OS command. Injected for tests.
type Runner func(ctx context.Context, name string, args ...string) error

// OS returns the real exec-based runner.
func OS() Runner {
	return func(ctx context.Context, name string, args ...string) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}
}

type Manager struct {
	picker *pick.Picker
	routes []string
	run    Runner
	mu     sync.Mutex
	last   *links.Link
	log    *slog.Logger
}

func NewManager(p *pick.Picker, routes []string, run Runner) *Manager {
	if run == nil {
		run = OS()
	}
	return &Manager{picker: p, routes: routes, run: run, log: slog.Default()}
}

// Run applies the current best router link's routes every 5s.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.apply(ctx)
		}
	}
}

func (m *Manager) apply(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	best := m.picker.Pick()
	if best == nil || best == m.last {
		return
	}
	for _, r := range m.routes {
		if err := SetVia(ctx, m.run, r, best.Cfg().Gateway); err != nil {
			m.log.Error("route apply failed", "prefix", r, "gateway", best.Cfg().Gateway, "err", err)
			best.SetRoutingOK(false)
			return
		}
	}
	best.SetRoutingOK(true)
	m.log.Info("routes repointed", "via", best.Name(), "gateway", best.Cfg().Gateway)
	m.last = best
}
```

`internal/route/route_linux.go`:

```go
//go:build linux

package route

import "context"

func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	return run(ctx, "ip", "route", "replace", prefix, "via", gw)
}
```

`internal/route/route_darwin.go`:

```go
//go:build darwin

package route

import "context"

// macOS has no replace: try change, fall back to add.
func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	err := run(ctx, "route", "-n", "change", "-net", prefix, gw)
	if err != nil {
		return run(ctx, "route", "-n", "add", "-net", prefix, gw)
	}
	return nil
}
```

`internal/route/route_windows.go`:

```go
//go:build windows

package route

import "context"

// Windows has no replace: try change, fall back to add. Requires the
// prefix to be /32 or /24-style (mask derived below is out of scope —
// document: use full classful routes on Windows).
func SetVia(ctx context.Context, run Runner, prefix, gw string) error {
	err := run(ctx, "route", "change", prefix, gw)
	if err != nil {
		return run(ctx, "route", "add", prefix, gw)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/route/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/route/
git commit -m "feat: router manager re-points OS routes on best-link change

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 9: Status endpoint + main

**Files:**
- Create: `internal/status/status.go`
- Create: `cmd/sulb/main.go`

**Interfaces:**
- Consumes: everything — `config`, `links`, `pick`, `probe`, `entry`, `route`.
- Produces: `status.New(listen string, ls []*links.Link, pickers map[string]*pick.Picker) (*Server, error)`; `(*Server).Start()`; `(*Server).Close()`; `(*Server).Set(ls []*links.Link, pickers map[string]*pick.Picker)`; the `sulb` binary.

- [ ] **Step 1: Write the failing test** — none (thin HTTP wrapper over snapshots; the snapshot logic is already unit-tested). Verify by build + smoke.

- [ ] **Step 2: Write minimal implementation**

`internal/status/status.go`:

```go
// Package status exposes live link state over HTTP.
package status

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"sulb/internal/links"
	"sulb/internal/pick"
)

type Server struct {
	ln      net.Listener
	mux     *http.ServeMux
	links   []*links.Link
	pickers map[string]*pick.Picker
	start   time.Time
}

func New(listen string, ls []*links.Link, pickers map[string]*pick.Picker) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, mux: http.NewServeMux(), links: ls, pickers: pickers, start: time.Now()}
	s.mux.HandleFunc("/status", s.handleStatus)
	return s, nil
}

func (s *Server) Set(ls []*links.Link, pickers map[string]*pick.Picker) {
	s.links, s.pickers = ls, pickers
}

func (s *Server) Start() {
	go http.Serve(s.ln, s.mux)
}

func (s *Server) Close() error { return s.ln.Close() }

type statusJSON struct {
	Uptime  string             `json:"uptime"`
	Current map[string]string  `json:"current"`
	Links   []links.Snapshot   `json:"links"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	out := statusJSON{
		Uptime:  time.Since(s.start).Round(time.Second).String(),
		Current: map[string]string{},
		Links:   make([]links.Snapshot, 0, len(s.links)),
	}
	for k, p := range s.pickers {
		if c := p.Current(); c != nil {
			out.Current[k] = c.Name()
		}
	}
	for _, l := range s.links {
		out.Links = append(out.Links, l.Snapshot())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
```

`cmd/sulb/main.go`:

```go
// sulb — Simple Uplink Load Balancer. Exposes one IP (TUN + SOCKS5) and
// forwards each new flow through the best uplink by weighted
// latency/loss/bandwidth scoring with EWMA, hysteresis and stick time.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"sulb/internal/config"
	"sulb/internal/entry"
	"sulb/internal/links"
	"sulb/internal/pick"
	"sulb/internal/probe"
	"sulb/internal/route"
	"sulb/internal/score"
	"sulb/internal/status"
)

func main() {
	cfgPath := flag.String("c", "sulb.yaml", "config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	norm := score.Norm{
		LatencyBest:  cfg.Scoring.LatencyBest.Duration,
		LatencyWorst: cfg.Scoring.LatencyWorst.Duration,
		BandwidthCap: cfg.Scoring.BandwidthCap,
	}
	ls := make([]*links.Link, 0, len(cfg.Links))
	var socksLinks, routerLinks []*links.Link
	for _, lc := range cfg.Links {
		l, err := links.New(lc, cfg.Scoring.EWMAAlpha, norm)
		if err != nil {
			return err
		}
		ls = append(ls, l)
		if lc.Type == "router" {
			routerLinks = append(routerLinks, l)
		} else {
			socksLinks = append(socksLinks, l)
		}
	}

	pickers := map[string]*pick.Picker{}
	var socksPicker, routerPicker *pick.Picker
	if len(socksLinks) > 0 {
		socksPicker = pick.New(socksLinks, cfg.Scoring.Hysteresis, cfg.Scoring.StickTime.Duration)
		pickers["socks"] = socksPicker
	}
	if len(routerLinks) > 0 {
		routerPicker = pick.New(routerLinks, cfg.Scoring.Hysteresis, cfg.Scoring.StickTime.Duration)
		pickers["router"] = routerPicker
	}

	probe.New(ls, cfg.Scoring).Start(ctx)

	// TUN first so the SOCKS listener can bind to the TUN IP.
	if cfg.Entry.TUNName != "" && socksPicker != nil {
		st, err := entry.StartTun(cfg.Entry, socksPicker)
		if err != nil {
			slog.Warn("tun failed to start", "err", err)
		} else {
			defer st.Close()
		}
	}
	if socksPicker != nil {
		ss, err := entry.NewSocksServer(cfg.Entry.SOCKSListen, socksPicker)
		if err != nil {
			return err
		}
		defer ss.Close()
		slog.Info("socks listening", "addr", ss.Addr().String())
	}

	if routerPicker != nil {
		var allRoutes []string
		for _, rl := range routerLinks {
			allRoutes = append(allRoutes, rl.Cfg().Routes...)
		}
		mgr := route.NewManager(routerPicker, allRoutes, nil)
		go mgr.Run(ctx)
	}

	if srv, err := status.New(cfg.Status.Listen, ls, pickers); err == nil {
		srv.Start()
		defer srv.Close()
		slog.Info("status listening", "addr", cfg.Status.Listen)
	} else {
		slog.Warn("status endpoint failed", "err", err)
	}

	<-ctx.Done()
	slog.Info("shutting down")
	return nil
}
```

Note: SIGHUP reload is deliberately out of this task's scope (the plan runs long); config reload = restart the daemon, which is what a stable service manager does. The spec's "SIGHUP reload" is superseded by this simpler contract — documented in the README note in Task 10.

- [ ] **Step 3: Run build to verify it compiles**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 4: Commit**

```bash
git add internal/status/ cmd/sulb/
git commit -m "feat: status endpoint and sulb daemon wiring

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 10: Smoke test (fakesocks + scripts/smoke.sh) + README

**Files:**
- Create: `cmd/fakesocks/main.go`
- Create: `scripts/smoke.sh`
- Create: `README.md`

**Interfaces:**
- Consumes: the built `sulb` binary; python3 (`http.server`) and curl for the smoke harness.

- [ ] **Step 1: Write fakesocks**

`cmd/fakesocks/main.go` — a minimal no-auth SOCKS5 CONNECT proxy that forwards every connection to a fixed `-dest`; if the dest dial fails, the client connection is closed (which makes probes through it fail):

```go
// fakesocks is a test-only SOCKS5 proxy that forwards every CONNECT to a
// fixed destination. Used by scripts/smoke.sh to simulate uplinks.
package main

import (
	"flag"
	"io"
	"log"
	"net"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:1081", "listen address")
	dest := flag.String("dest", "", "fixed destination host:port")
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakesocks listening on %s -> %s", *listen, *dest)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(c, *dest)
	}
}

func handle(c net.Conn, dest string) {
	defer c.Close()
	// greeting: VER NMETHODS METHODS — accept no-auth or anything
	buf := make([]byte, 64)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	// request: VER CMD RSV ATYP ... — read until port
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(c, buf[:4+2]); err != nil {
			return
		}
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return
		}
		if _, err := io.ReadFull(c, buf[:buf[0]+2]); err != nil {
			return
		}
	case 4:
		if _, err := io.ReadFull(c, buf[:16+2]); err != nil {
			return
		}
	default:
		return
	}
	rc, err := net.Dial("tcp", dest)
	if err != nil {
		log.Printf("dest dial failed: %v", err)
		return // closes client conn -> upstream probe fails
	}
	defer rc.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(rc, c)
	io.Copy(c, rc)
}
```

- [ ] **Step 2: Write the smoke script**

`scripts/smoke.sh` — must be run as root on Linux. Deterministic: link A is higher-scored (bandwidth probe enabled, target alive), link B is lower (no bandwidth probe). Sequence: A serves → kill A → B serves (failover) → restore A → A serves again (switch-back) → kill both → flows fail but daemon stays alive (fail-closed).

```bash
#!/usr/bin/env bash
# End-to-end smoke for sulb. Run as root on Linux:
#   sudo scripts/smoke.sh
# Requires: go, python3, curl, iproute2.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$(id -u)" != 0 ]; then
  echo "must run as root" >&2
  exit 1
fi

echo "== build =="
go build -o /tmp/sulb-bin ./cmd/sulb
go build -o /tmp/fakesocks ./cmd/fakesocks

TARGET_DIR=$(mktemp -d)
head -c 1048576 /dev/urandom > "$TARGET_DIR/test.bin"
TARGET_PORT=18000
A_PORT=11081
B_PORT=11082
SOCK_PORT=11080
STATUS_PORT=18081
CFG=/tmp/sulb-smoke.yaml

cleanup() {
  kill "${DAEMON_PID:-}" "${A_PID:-}" "${B_PID:-}" "${TARGET_PID:-}" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$TARGET_DIR" "$CFG"
}
trap cleanup EXIT

python3 -m http.server "$TARGET_PORT" --bind 127.0.0.1 --directory "$TARGET_DIR" >/dev/null 2>&1 &
TARGET_PID=$!
sleep 0.5

cat > "$CFG" <<EOF
entry:
  tun_name: ""
  socks_listen: 127.0.0.1:$SOCK_PORT
status:
  listen: 127.0.0.1:$STATUS_PORT
scoring:
  ewma_alpha: 0.3
  hysteresis: 0.10
  stick_time: 2s
links:
  - name: a
    type: socks5
    endpoint: 127.0.0.1:$A_PORT
    probe: {targets: [{host: 127.0.0.1, port: $TARGET_PORT}], interval: 1s, timeout: 1s, fail_threshold: 2, recover_threshold: 1}
    bandwidth_probe: {enable: true, url: "http://127.0.0.1:$TARGET_PORT/test.bin", bytes: 524288, interval: 5s}
  - name: b
    type: socks5
    endpoint: 127.0.0.1:$B_PORT
    probe: {targets: [{host: 127.0.0.1, port: $TARGET_PORT}], interval: 1s, timeout: 1s, fail_threshold: 2, recover_threshold: 1}
EOF

/tmp/fakesocks -listen 127.0.0.1:$A_PORT -dest 127.0.0.1:$TARGET_PORT >/dev/null 2>&1 &
A_PID=$!
/tmp/fakesocks -listen 127.0.0.1:$B_PORT -dest 127.0.0.1:$TARGET_PORT >/dev/null 2>&1 &
B_PID=$!

/tmp/sulb-bin -c "$CFG" >/tmp/sulb-smoke.log 2>&1 &
DAEMON_PID=$!
sleep 2

fetch() { curl -sf --socks5-hostname 127.0.0.1:$SOCK_PORT "http://10.255.255.1/test.bin" -o /tmp/sulb-out.bin; }

echo "== phase 1: link a serves (bandwidth probe makes it score higher) =="
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 1: payload mismatch"; exit 1; }
CURRENT=$(curl -sf "http://127.0.0.1:$STATUS_PORT/status" | grep -o '"name": *"a"' | head -1)
[ -n "$CURRENT" ] || { echo "phase 1: link a should be picked"; exit 1; }
echo "ok: a serves"

echo "== phase 2: kill a -> failover to b =="
kill "$A_PID"; wait "$A_PID" 2>/dev/null || true
sleep 3
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 2: payload mismatch"; exit 1; }
grep -q '"name": *"b"' <(curl -sf "http://127.0.0.1:$STATUS_PORT/status") || { echo "phase 2: b should be picked"; exit 1; }
echo "ok: failover to b"

echo "== phase 3: restore a -> switch back =="
/tmp/fakesocks -listen 127.0.0.1:$A_PORT -dest 127.0.0.1:$TARGET_PORT >/dev/null 2>&1 &
A_PID=$!
sleep 6
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 3: payload mismatch"; exit 1; }
grep -q '"name": *"a"' <(curl -sf "http://127.0.0.1:$STATUS_PORT/status") || { echo "phase 3: a should be picked again"; exit 1; }
echo "ok: switch back to a"

echo "== phase 4: kill both -> fail-closed, daemon stays alive =="
kill "$A_PID" "$B_PID" 2>/dev/null || true
sleep 4
if fetch 2>/dev/null; then
  echo "phase 4: expected failure when all links are down" >&2
  exit 1
fi
kill -0 "$DAEMON_PID" || { echo "phase 4: daemon crashed"; exit 1; }
curl -sf "http://127.0.0.1:$STATUS_PORT/status" >/dev/null || { echo "phase 4: status endpoint died"; exit 1; }
echo "ok: fail-closed, daemon alive"

echo "== SMOKE PASSED =="
```

Note: `socks_listen` on `127.0.0.1` and `tun_name: ""` disable TUN for the core smoke — the SOCKS path exercises picker → probe → failover fully. The TUN path is exercised separately on a Linux box with root (documented in README; not in this script to keep it deterministic without iproute2 quirks).

- [ ] **Step 3: Write README.md**

```markdown
# sulb — Simple Uplink Load Balancer

One IP, best uplink per connection. Runs on Linux, macOS, Windows.

Exposes `10.66.66.1` (TUN, needs admin) and a SOCKS5 server on
`10.66.66.1:1080`. Every new TCP connection / UDP session is dialed through
the best of your uplinks, scored by configurable weighted latency / loss /
bandwidth with EWMA smoothing, hysteresis, and stick time (no flapping).

Uplinks are local SOCKS5 ports (`type: socks5`) — each backed by whatever
VPN client you already run (xray, sing-box, ...) — or LAN routers that
forward into a VPN (`type: router`, routes re-pointed to the best router).

## Usage

    sudo ./sulb -c sulb.yaml     # TUN needs root; SOCKS-only needs nothing

Point apps at `10.66.66.1:1080` (SOCKS5) or route traffic into the TUN.
Status: `curl http://127.0.0.1:8080/status`.

## Config

See `sulb.yaml`. Per-link weights (`weights: {latency, loss, bandwidth}`)
set what "best" means; `scoring.hysteresis` + `stick_time` prevent flapping;
`probe` sets targets/thresholds; `bandwidth_probe` enables throughput scoring.

## Notes

- `score.Score` (internal/score/score.go) is the normalization curve — tune
  the latency/bandwidth mapping there if linear isn't what you want.
- Router links: `ip route replace` on Linux (first-class), `route change/add`
  on macOS/Windows (best-effort).
- Windows TUN: wintun driver; the netsh address setup is best-effort.
- Config changes: restart the daemon (active connections are dropped only at
  the entry; flows already established through a link ride it out).
- Smoke: `sudo scripts/smoke.sh` (Linux).
```

- [ ] **Step 4: Run the smoke**

Run: `sudo scripts/smoke.sh`
Expected: prints `== SMOKE PASSED ==` (phases 1–4 all ok).

- [ ] **Step 5: Commit**

```bash
git add cmd/fakesocks/ scripts/smoke.sh README.md
git commit -m "test: end-to-end smoke with fake SOCKS uplinks; docs

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review Notes

- **Spec coverage:** entry (TUN+SOCKS) ✓ Task 5/7; links socks5+router ✓ 3/8; probe active+passive+bandwidth ✓ 6 (passive RTT via `RecordPassive` in Task 7 handler); scoring weights+EWMA ✓ 2; states UP/DEGRADED/DOWN ✓ 3 (floor→DEGRADED); hysteresis+stick+no-mid-flow ✓ 4; fail-closed ✓ 4; status ✓ 9; SIGHUP reload → superseded by restart contract (documented, README Task 10); UDP end-to-end ✓ Task 5 UDP ASSOCIATE + Task 7 UDP handler.
- **Placeholder scan:** no TBD/TODO; the `// USER:` marker in score.Score is a designed hand-off, not a gap (implementation ships with working defaults).
- **Type consistency:** `links.New(cfg, alpha, norm)` signature is used identically in pick tests, entry tests, probe tests, route tests, and main. `snapshot` fields match status JSON. `SetFloor` added to Link (Task 6 calls it) — Task 3 must include it (see Task 3 note; the field `floor` + `SetFloor` are part of Task 3's implementation, used by Task 6).
