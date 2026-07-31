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
