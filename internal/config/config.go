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
	// TUNName is NOT defaulted: empty string disables TUN. The shipped
	// sulb.yaml sets it explicitly. (macOS requires a utun* name.)
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
