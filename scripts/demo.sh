#!/usr/bin/env bash
# demo.sh — stand up a tiny veth topology and run the vibe-switch binary on it,
# then ping across it and dump the FDB via `ctl`. Needs root (netns/veth/AF_PACKET).
#
#   sudo scripts/demo.sh
#
# Two hosts (netns demoA/demoB) wired to switch-side veth ports sw1/sw2 in the
# root netns. The switch runs as the plain binary — no test harness involved.
set -euo pipefail

SOCK=${SOCK:-/run/vibe-switch-demo.sock}
BIN=${BIN:-./bin/vibe-switch}

cleanup() {
	[ -n "${SWPID:-}" ] && kill -INT "$SWPID" 2>/dev/null || true
	ip netns del demoA 2>/dev/null || true
	ip netns del demoB 2>/dev/null || true
	ip link del sw1 2>/dev/null || true
	ip link del sw2 2>/dev/null || true
}
trap cleanup EXIT

[ -x "$BIN" ] || { echo "build first: make build-bin"; exit 1; }

# Fresh topology.
cleanup
ip netns add demoA
ip netns add demoB
ip link add sw1 type veth peer name e1
ip link add sw2 type veth peer name e2
ip link set e1 netns demoA
ip link set e2 netns demoB
ip link set sw1 up
ip link set sw2 up
sysctl -qw net.ipv6.conf.sw1.disable_ipv6=1
sysctl -qw net.ipv6.conf.sw2.disable_ipv6=1
ip netns exec demoA sh -c 'ip link set e1 name eth0; ip link set eth0 address 02:00:00:00:00:01; sysctl -qw net.ipv6.conf.eth0.disable_ipv6=1; ip addr add 10.0.0.1/24 dev eth0; ip link set eth0 up'
ip netns exec demoB sh -c 'ip link set e2 name eth0; ip link set eth0 address 02:00:00:00:00:02; sysctl -qw net.ipv6.conf.eth0.disable_ipv6=1; ip addr add 10.0.0.2/24 dev eth0; ip link set eth0 up'

# Run the switch in the background.
"$BIN" -i sw1 -i sw2 -ctl-sock "$SOCK" &
SWPID=$!
sleep 1

echo "=== ping demoA -> demoB (through the Go switch) ==="
ip netns exec demoA ping -c 2 -W 1 10.0.0.2

echo "=== ctl show fdb ==="
"$BIN" ctl -ctl-sock "$SOCK" show fdb
echo "=== ctl show stats ==="
"$BIN" ctl -ctl-sock "$SOCK" show stats
