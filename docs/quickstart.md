# Quickstart

Install sulb, point your apps at the exposed IP, done. One binary per OS —
grab it from the [releases page](https://github.com/NotABaguette/SULB/releases)
or build it: `go build -o sulb ./cmd/sulb`.

## 1. Config

Copy `sulb.yaml` and edit the `links:` list — each entry is one of your
uplinks. The two link types:

```yaml
links:
  - name: vpn-a          # any name you like
    type: socks5         # a local SOCKS5 port (xray/sing-box/VPN client)
    endpoint: 127.0.0.1:1081
    probe:
      targets: [{host: 1.1.1.1, port: 443}]   # health-checked through this link

  - name: router-1
    type: router         # a LAN router that forwards into a VPN
    gateway: 192.168.1.2
    routes: [0.0.0.0/0]  # prefixes re-pointed via the best router
```

## 2. Run

| OS | Command | Notes |
|---|---|---|
| Linux | `sudo ./sulb -c sulb.yaml` | TUN name `sulb0` works; needs root |
| macOS | `sudo ./sulb -c sulb.yaml` | TUN name must be `utun3`-style (`entry.tun_name: utun9`); needs root |
| Windows | `sulb.exe -c sulb.yaml` | wintun driver; run as admin; `netsh` address setup is best-effort |

Don't want the TUN at all? Set `entry.tun_name: ""` — the SOCKS5 server
still works, no root needed.

## 3. Use it

- **SOCKS5 proxy:** point apps at `10.66.66.1:1080` (or whatever
  `entry.socks_listen` says). Example: `curl --socks5-hostname 10.66.66.1:1080 https://example.com`
- **TUN:** route traffic into `10.66.66.1` — the daemon creates the
  interface with that address on startup.

## 4. Verify

```sh
curl http://127.0.0.1:8080/status
```

Shows per-link state (up/degraded/down), score, latency, loss, bandwidth,
and which link is currently serving:

```json
{"uptime":"2m","current":{"socks":"vpn-a"},
 "links":[{"name":"vpn-a","state":2,"score":97.8,"latency":24500000,...}]}
```

## Running as a service

**Linux (systemd)** — `/etc/systemd/system/sulb.service`:

```ini
[Unit]
Description=sulb uplink load balancer
After=network-online.target

[Service]
ExecStart=/usr/local/bin/sulb -c /etc/sulb/sulb.yaml
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now sulb
```

**macOS (launchd)** — `~/Library/LaunchAgents/com.sulb.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.sulb</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/sulb</string><string>-c</string><string>/usr/local/etc/sulb.yaml</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
```

```sh
launchctl load ~/Library/LaunchAgents/com.sulb.plist
```

**Windows:** run `sulb.exe` as a scheduled task (or `sc create` with the
`runas` service account); the TUN needs admin privileges.

## Troubleshooting

- **`tun failed to start`** in the log — the daemon continues SOCKS-only.
  Check the TUN name (macOS: `utun*` only), root/admin rights, wintun
  driver on Windows.
- **Link stuck `down`** — check the probe target is reachable *through*
  that link, and that `fail_threshold`/`recover_threshold` fit the link's
  real flakiness.
- **Never switches back** — check `scoring.hysteresis` and `stick_time`;
  an unmeasured link (no `bandwidth_probe`) caps at a lower score by
  design, so a bandwidth-proven link wins.
