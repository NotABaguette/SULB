package config

import (
	"os"
	"path/filepath"
	"strings"
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
		{"negative weights", "links:\n  - {name: a, type: socks5, endpoint: 127.0.0.1:1, weights: {latency: -1, loss: 0, bandwidth: 0.3}}\n", "weights"},
		// note: all-zero weights are indistinguishable from unset and get defaults
		{"bad target host", "links:\n  - {name: a, type: socks5, endpoint: 127.0.0.1:1, probe: {targets: [{host: notanip, port: 443}]}}\n", "target"},
		{"bad route", "links:\n  - {name: a, type: router, gateway: 192.168.1.2, routes: [nope]}\n", "route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
