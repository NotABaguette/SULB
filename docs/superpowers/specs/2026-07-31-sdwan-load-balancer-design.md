# SULB — Simple Uplink Load Balancer (SD-WAN for SOCKS + router links)

Date: 2026-07-31
Status: Approved (design §1–§3)

## Problem

The user has multiple VPN uplinks on one machine, each reachable differently:
- local SOCKS5 proxy ports (each backed by a VPN client like xray/sing-box running locally),
- routers on the LAN that forward packets into their respective VPNs (next-hop links).

They want one exposed IP that receives incoming packets (TCP + UDP) and forwards
each new connection through the **best** uplink, where "best" is a configurable
weighted function of latency, loss, and bandwidth. It must run on **Linux, macOS,
and Windows**, be **super stable** (no flapping, no mid-flow switching, no crash on
link failure) and **super accurate** (real measurements — active probes plus passive
real-traffic stats — not guesses).

## Decisions (confirmed with user)

1. **Platform:** Linux + macOS + Windows → userspace datapath (TUN), not kernel
   policy routing (Linux-only).
2. **Links:** primary type is `socks5` — the local VPN clients already run and each
   exposes a SOCKS5 port. Secondary type is `router` — a LAN gateway IP that forwards
   packets into a VPN (route-based link).
3. **Entry:** TUN interface with a dedicated IP (default `10.66.66.1/24`) using
   gVisor netstack via the tun2socks library, **plus** a SOCKS5 listener bound to the
   same IP:port (default `:1080`).
4. **Traffic:** TCP + UDP end-to-end.
5. **Approach:** custom Go daemon — own scoring engine; battle-tested packet path
   reused from tun2socks (netstack), not written from scratch.
6. **Product name:** sulb (from the user's reference repo).

## Architecture

```
config.yaml
     │
┌────▼──────────────────────────────────────────────────┐
│  sulb daemon (one Go binary, no runtime deps)         │
│                                                       │
│  ENTRY (exposed IP 10.66.66.1)                        │
│   • TUN 10.66.66.1/24 — gVisor netstack (tun2socks    │
│     library), TCP+UDP termination                     │
│   • SOCKS5 :1080 bound to 10.66.66.1                  │
│            │        │                                 │
│            ▼        ▼                                 │
│       LinkPicker (per new flow)                       │
│            │                                          │
│            ▼                                          │
│  Dialers:  socks5://127.0.0.1:1081   router://192.168.1.2
│            ▲                                          │
│  ProbeEngine (per-link goroutines)                    │
│   active:  TCP connect / HTTP GET / UDP echo /        │
│            bandwidth download sample                  │
│   passive: real-conn RTT, per-conn throughput         │
│            │                                          │
│            ▼                                          │
│  ScoringEngine: normalize → weights → EWMA →          │
│  UP/DEGRADED/DOWN + hysteresis + stick time           │
│                                                       │
│  RouterManager: ip route / route add via chosen       │
│  router (Linux full, macOS/Win best-effort)           │
└───────────────────────────────────────────────────────┘
```

### Components

1. **`entry`** — TUN via tun2socks library (custom handler plugged in) + in-house
   SOCKS5 server (~80 lines). Both hand new connections to the picker.
2. **`links`** — registry: `{name, type: socks5|router, endpoint, probe config,
   weights}`. Dialers: SOCKS5 client (`golang.org/x/net/proxy`), router links dial
   via OS routes.
3. **`probe`** — per-link goroutine, active + passive collection, own timeouts.
   Never blocks the picker (picker reads cached scores).
4. **`score`** — pure function, no I/O: metrics → score. Unit-testable.
5. **`pick`** — per new connection/session: best UP link. **No mid-flow switching.**
6. **`status`** — HTTP endpoint on 127.0.0.1 (port configurable) showing per-link
   live state (UP/DEGRADED/DOWN, current metrics, score, chosen link).

### Data flow

Connection arrives on TUN or SOCKS5 → `pick()` → dial through chosen link →
`io.Copy` both directions, sampling RTT/bytes → probes feed scoring → state
changes affect **new** flows only. Existing connections ride their original link.

## Link states

```
DOWN ◄── N consecutive probe failures (fail_threshold)
  ▲ │
  │ ▼
UP ◄── M consecutive successes (recover_threshold)
  │
  ▼
DEGRADED — score below floor but probes still pass (configurable)
```

## Metrics

| Metric | Measurement |
|---|---|
| latency | TCP connect to probe target(s) *through* the link; passive RTT of real connections (EWMA) |
| loss | % of failed probes in the last window |
| bandwidth | optional periodic download sample through the link (configurable bytes/interval) |
| score floor | below it → DEGRADED; used only if nothing better |

## Scoring

- Pure function: each metric normalized 0–100 (latency/loss inverted), weighted sum
  from config (e.g. `{latency: 0.5, loss: 0.2, bandwidth: 0.3}`).
- EWMA smoothing (`ewma_alpha`) — one bad probe must not tank a good link.
- `scoreLink(link, metrics, weights) → score` — the one function the user will shape
  at implementation time (normalization aggressiveness of bandwidth vs latency).

## Stability guarantees

1. **Hysteresis** — switch only if candidate score exceeds current by a margin
   (default 10%).
2. **Stick time** — minimum seconds on a link before switching (default 15s).
3. **No mid-flow switching** — established flows keep their link.
4. **Fail-closed ordering** — UP > DEGRADED > DOWN; all DOWN → least-bad with loud
   logging and status visible on `/status`.

## Error handling

- Probe errors → state machine only, never crash.
- TUN failure → auto-recreate + retry loop; daemon stays up.
- Router route command failure → link marked DOWN *for routing only*; SOCKS dialers
  unaffected.
- SIGHUP re-reads config without dropping active connections.

## Config (sulb.yaml)

```yaml
entry:
  tun_ip: 10.66.66.1
  tun_net: 24
  socks_port: 1080

scoring:
  ewma_alpha: 0.3
  hysteresis: 0.10      # switch only if candidate beats current by 10%
  stick_time: 15s

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
    bandwidth_probe: {enable: true, bytes: 524288, interval: 60s}

  - name: vpn-b
    type: router
    gateway: 192.168.1.2
    routes: [0.0.0.0/0]
    probe:
      targets: [{host: 1.1.1.1, port: 443}]
      interval: 5s
```

## Repo layout

```
sdwan/
  go.mod
  cmd/sulb/main.go        # flags, config load, wiring, SIGHUP
  internal/entry/         # TUN (tun2socks) + socks5 server
  internal/links/         # registry, dialers (socks5 client, router)
  internal/probe/         # active + passive probes
  internal/score/         # scoreLink() — user-shaped, pure function
  internal/pick/          # link picker
  internal/status/        # /status HTTP endpoint
  sulb.yaml               # default config
  scripts/smoke.sh        # integration smoke on Linux
```

## Dependencies (exactly three)

- `github.com/xjasonlyu/tun2socks/v2` — TUN + netstack, used as a library
- `gopkg.in/yaml.v3` — config
- `golang.org/x/net` — SOCKS5 client

SOCKS5 server written in-house (~80 lines) — no extra dependency.

## Testing

- `score` and the state machine: one small assert-based test each. Critical case:
  flap suppression — hysteresis + stick time hold under oscillating probe results.
- `scripts/smoke.sh` (Linux): two local fake SOCKS servers → run daemon → verify
  connections land on the higher-scoring link → kill it → verify failover → restore
  → verify switch-back. Real end-to-end, no mocks.
- Cross-OS: TUN creation is the only OS-specific surface — documented per OS
  (`sudo` on macOS/Linux, wintun driver on Windows).

## Deliverable

One `sulb` binary per OS (linux/darwin/windows), config-driven, no other moving
parts. Requires admin/root at runtime for TUN creation only.

## Out of scope (v1)

- VLESS protocol dialing inside the daemon (all VPN clients already run locally
  and expose SOCKS5 — link is the SOCKS port).
- Kernel-datapath mode (nftables/policy routing) — Linux-only, future option.
- LAN-client L3 forwarding (the exposed IP serves apps on the balancer machine;
  LAN clients can use it as a SOCKS proxy).
