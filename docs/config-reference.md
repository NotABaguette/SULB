# Config reference

All keys optional unless marked. Defaults shown inline.

## `entry` — the exposed IP

```yaml
entry:
  tun_name: sulb0        # "" disables the TUN. macOS needs a utun* name.
  tun_ip: 10.66.66.1     # IP assigned to the TUN interface
  tun_net: 24            # prefix length
  mtu: 1500
  socks_listen: 10.66.66.1:1080   # SOCKS5 server (same IP:port style)
```

## `scoring` — what "best link" means

```yaml
scoring:
  ewma_alpha: 0.3     # smoothing: 0.3 = one bad probe moves score 30%
  hysteresis: 0.10    # switch only if candidate beats current by 10%
  stick_time: 15s     # minimum time on a healthy link before switching
  latency_best: 10ms  # latency at score 100
  latency_worst: 300ms# latency at score 0 (linear between)
  bandwidth_cap: 10485760  # bytes/sec at score 100 (linear)
  floor: 0            # score below this -> DEGRADED state; 0 = disabled
```

How a link is scored:

- **latency** — TCP connect to each `probe.target` *through the link*,
  EWMA-smoothed. Linear between `latency_best` (100) and `latency_worst` (0).
- **loss** — failed probes in the last `loss_window` probes, as a fraction.
  0% loss scores 100.
- **bandwidth** — periodic download of `bandwidth_probe.bytes` through the
  link, measured in bytes/sec, linear to `bandwidth_cap`.
- `score = (w_lat·lat + w_loss·loss + w_bw·bw) / (w_lat + w_loss + w_bw)`
- **A metric that isn't measured contributes 0** — no renormalization. A
  link without `bandwidth_probe` caps at `(w_lat + w_loss)` of 100, so a
  bandwidth-proven link beats it. This is deliberate.

Stability: the picker only switches when the candidate's score exceeds the
current link's by `hysteresis` **and** `stick_time` has elapsed. A link that
goes DOWN is failed over immediately, ignoring both.

## `links` — your uplinks

```yaml
links:
  - name: vpn-a                       # required, unique
    type: socks5                      # socks5 | router | direct
    endpoint: 127.0.0.1:1081          # socks5: required
    gateway: 192.168.1.2              # router: required
    routes: [0.0.0.0/0]               # router: prefixes routed via it
    weights: {latency: 0.5, loss: 0.2, bandwidth: 0.3}   # defaults
    probe:
      targets: [{host: 1.1.1.1, port: 443}, {host: 8.8.8.8, port: 53}]
      interval: 5s
      timeout: 2s
      fail_threshold: 3    # consecutive failures -> DOWN
      recover_threshold: 2 # consecutive successes -> UP again
      loss_window: 10      # probe history for the loss metric
    bandwidth_probe:       # optional; enables the bandwidth score
      enable: true
      url: "http://speed.cloudflare.com/__down?bytes=524288"
      bytes: 524288
      interval: 60s
```

Link types:

| type | endpoint | meaning |
|---|---|---|
| `socks5` | `host:port` | dial through a local SOCKS5 proxy (VPN client) |
| `router` | `gateway` + `routes` | L3 traffic for `routes` is routed via the best gateway (`ip route replace` on Linux, `route change/add` elsewhere) |
| `direct` | — | dial directly, no proxy (useful for testing) |

`router` links don't dial connections — they manage OS routes. SOCKS/direct
links serve the TUN and SOCKS entries.

## `status` — observability

```yaml
status:
  listen: 127.0.0.1:8080   # GET /status -> per-link JSON
```

## Example: latency-first

```yaml
scoring: {latency_best: 5ms, latency_worst: 200ms, stick_time: 30s}
links:
  - {name: vpn-a, type: socks5, endpoint: 127.0.0.1:1081,
     weights: {latency: 0.8, loss: 0.2, bandwidth: 0}}
  - {name: vpn-b, type: socks5, endpoint: 127.0.0.1:1082,
     weights: {latency: 0.8, loss: 0.2, bandwidth: 0}}
```

## Example: bandwidth-first with a failover router

```yaml
scoring: {bandwidth_cap: 20971520, stick_time: 60s}
links:
  - {name: vpn-a, type: socks5, endpoint: 127.0.0.1:1081,
     weights: {latency: 0.1, loss: 0.1, bandwidth: 0.8},
     bandwidth_probe: {enable: true}}
  - {name: router-1, type: router, gateway: 192.168.1.2, routes: [0.0.0.0/0],
     weights: {latency: 0.5, loss: 0.5, bandwidth: 0}}
```

## Notes

- **The normalization curve** (`latency_best/worst`, `bandwidth_cap`) is
  linear. If you want log-scale bandwidth or a "good enough under 50ms"
  latency curve, `score.Score` in `internal/score/score.go` is the single
  place to change it (marked `// USER:`).
- Config changes take effect on restart. Established flows ride their
  original link either way.
- Probe targets must be IPs (no hostnames) in `probe.targets`.
