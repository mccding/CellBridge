#!/bin/sh

set -eu

NETNS="cb-fnos-web"
HOST_IF="cbfns0"
NS_IF="cbfns1"
HOST_ADDR="169.254.251.1/30"
NS_ADDR="169.254.251.2/30"
NS_IP="169.254.251.2"
NGINX="/usr/trim/nginx/sbin/nginx"
NAT_CHAIN="CB_FNOSWEB_NAT"
FILTER_CHAIN="CB_FNOSWEB_FILTER"
child=""

remove_jump() {
    table="$1"
    parent="$2"
    chain="$3"
    interface="${4:-}"
    if [ -n "$interface" ]; then
        while iptables -t "$table" -C "$parent" -i "$interface" -j "$chain" >/dev/null 2>&1; do
            iptables -t "$table" -D "$parent" -i "$interface" -j "$chain"
        done
    else
        while iptables -t "$table" -C "$parent" -j "$chain" >/dev/null 2>&1; do
            iptables -t "$table" -D "$parent" -j "$chain"
        done
    fi
}

cleanup_firewall() {
    set +e
    remove_jump nat PREROUTING "$NAT_CHAIN"
    remove_jump nat PREROUTING "$NAT_CHAIN" eth0
    remove_jump nat PREROUTING "$NAT_CHAIN" usb0
    remove_jump nat OUTPUT "$NAT_CHAIN"
    remove_jump filter FORWARD "$FILTER_CHAIN"
    iptables -t nat -F "$NAT_CHAIN" >/dev/null 2>&1
    iptables -t nat -X "$NAT_CHAIN" >/dev/null 2>&1
    iptables -t filter -F "$FILTER_CHAIN" >/dev/null 2>&1
    iptables -t filter -X "$FILTER_CHAIN" >/dev/null 2>&1
}

cleanup_netns() {
    set +e
    ip netns del "$NETNS" >/dev/null 2>&1
    ip link del "$HOST_IF" >/dev/null 2>&1
    set -e
}

cleanup() {
    cleanup_firewall
    cleanup_netns
}

on_signal() {
    trap - TERM INT EXIT
    if [ -n "$child" ]; then
        kill -TERM "$child" >/dev/null 2>&1 || true
        wait "$child" >/dev/null 2>&1 || true
    fi
    cleanup
    exit 0
}

trap on_signal TERM INT
trap cleanup EXIT

# Recover cleanly from an interrupted previous start.
cleanup

ip netns add "$NETNS"
ip link add "$HOST_IF" type veth peer name "$NS_IF"
ip link set "$NS_IF" netns "$NETNS"
ip addr add "$HOST_ADDR" dev "$HOST_IF"
ip link set "$HOST_IF" up
ip netns exec "$NETNS" ip link set lo up
ip netns exec "$NETNS" ip addr add "$NS_ADDR" dev "$NS_IF"
ip netns exec "$NETNS" ip link set "$NS_IF" up
ip netns exec "$NETNS" ip route replace default via 169.254.251.1

iptables -t nat -N "$NAT_CHAIN" 2>/dev/null || true
iptables -t nat -F "$NAT_CHAIN"
for interface in eth0 usb0; do
    if ip link show "$interface" >/dev/null 2>&1; then
        iptables -t nat -I PREROUTING 1 -i "$interface" -j "$NAT_CHAIN"
    fi
done
iptables -t nat -I OUTPUT 1 -j "$NAT_CHAIN"
for address in 10.0.0.211 192.168.225.24; do
    for port in 80 443 5666 5667; do
        iptables -t nat -A "$NAT_CHAIN" -d "$address/32" -p tcp --dport "$port" \
            -j DNAT --to-destination "$NS_IP:$port"
    done
done

iptables -t filter -N "$FILTER_CHAIN" 2>/dev/null || true
iptables -t filter -F "$FILTER_CHAIN"
iptables -t filter -I FORWARD 1 -j "$FILTER_CHAIN"
for interface in eth0 usb0; do
    if ip link show "$interface" >/dev/null 2>&1; then
        iptables -t filter -A "$FILTER_CHAIN" -i "$interface" -o "$HOST_IF" -d "$NS_IP/32" \
            -p tcp -m multiport --dports 80,443,5666,5667 -j ACCEPT
        iptables -t filter -A "$FILTER_CHAIN" -i "$HOST_IF" -o "$interface" -s "$NS_IP/32" \
            -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    fi
done
# Do not allow the isolated fnOS Web namespace to become a Tailnet path.
iptables -t filter -A "$FILTER_CHAIN" -o "$HOST_IF" -j DROP
iptables -t filter -A "$FILTER_CHAIN" -i "$HOST_IF" -o tailscale0 -j DROP

# The stock fnOS nginx remains untouched; its wildcard 80/443 listeners live
# only inside this namespace.  Unix-socket backends and /usr/trim/www remain
# available because the namespace isolates networking, not the filesystem.
ip netns exec "$NETNS" "$NGINX" -g 'daemon off;' &
child=$!

ready=0
attempt=0
while [ "$attempt" -lt 30 ]; do
    if ip netns exec "$NETNS" ss -ltn 2>/dev/null | grep -Eq '0\.0\.0\.0:(443|5667)'; then
        ready=1
        break
    fi
    attempt=$((attempt + 1))
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    kill -TERM "$child" >/dev/null 2>&1 || true
    wait "$child" >/dev/null 2>&1 || true
    exit 1
fi

set +e
wait "$child"
status=$?
set -e
exit "$status"
