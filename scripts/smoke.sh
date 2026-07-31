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
current_link() { curl -sf "http://127.0.0.1:$STATUS_PORT/status" | grep -o '"socks": *"'"$1"'"' | head -1; }

echo "== phase 1: link a serves (bandwidth probe makes it score higher) =="
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 1: payload mismatch"; exit 1; }
[ -n "$(current_link a)" ] || { echo "phase 1: link a should be picked"; exit 1; }
echo "ok: a serves"

echo "== phase 2: kill a -> failover to b =="
kill "$A_PID"; wait "$A_PID" 2>/dev/null || true
sleep 3
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 2: payload mismatch"; exit 1; }
[ -n "$(current_link b)" ] || { echo "phase 2: b should be picked"; exit 1; }
echo "ok: failover to b"

echo "== phase 3: restore a -> switch back =="
/tmp/fakesocks -listen 127.0.0.1:$A_PORT -dest 127.0.0.1:$TARGET_PORT >/dev/null 2>&1 &
A_PID=$!
# Switch-back depends on probe + bandwidth-probe cadence and EWMA settling
# (~7-8s). Selection only happens on traffic, so each poll iteration must
# generate a flow through the balancer — that's what triggers the re-pick.
for _ in $(seq 1 20); do
  fetch 2>/dev/null || true
  [ -n "$(current_link a)" ] && break
  sleep 1
done
fetch
cmp -s /tmp/sulb-out.bin "$TARGET_DIR/test.bin" || { echo "phase 3: payload mismatch"; exit 1; }
[ -n "$(current_link a)" ] || { echo "phase 3: a should be picked again"; exit 1; }
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
