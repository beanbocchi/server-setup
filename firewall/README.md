# Firewall — nftables + Docker Native

## Problem

Docker modifies iptables/nftables directly. When you publish a port (`-p 8080:80`), Docker creates DNAT rules that bypass traditional INPUT chain firewalls — the port becomes globally reachable even if UFW or iptables says it's blocked.

There is no way to control Docker-published ports from INPUT because Docker's DNAT rewrites the destination **before** INPUT sees the packet.

## Architecture

A single **gate chain** at `prerouting priority -150` — after conntrack (-200) so `ct state` works, before Docker DNAT (-100) so original destination ports are still visible. Every external packet hits this one checkpoint. The gate uses `policy drop` (fail-closed) — if rules get flushed or the chain breaks, all external traffic is blocked. No silent fail-open.

```
                    External packet
                         │
                         ▼
              ┌─────────────────────┐
              │  conntrack (-200)   │
              └────────┬────────────┘
                       │
                       ▼
              ┌─────────────────────┐
              │   ★ GATE (-150)     │  ← single choke point, policy drop
              │                     │
              │  trusted iface? ──────► ACCEPT (lo, br-*, docker0)
              │  established?   ──────► ACCEPT
              │  blocked IP?    ──────► DROP  (fail2ban + manual bans)
              │  allowed port?  ──────► ACCEPT
              │  everything else ─────► DROP (policy)
              └────────┬────────────┘
                       │ only accepted traffic
                       ▼
              ┌─────────────────────┐
              │  Docker DNAT (-100) │
              └────────┬────────────┘
                       │
                ┌──────┴──────┐
                ▼             ▼
           ┌────────┐   ┌──────────┐
           │ INPUT  │   │ FORWARD  │
           │ (host) │   │ (Docker) │
           └────────┘   └──────────┘
```

### Why priority -150

| Priority | Hook | Why this position |
|----------|------|-------------------|
| -200 | conntrack | Must come first so `ct state established` works in our gate |
| **-150** | **gate** | **After conntrack, before Docker DNAT — sees original dest port** |
| -100 | Docker DNAT | Rewrites port to container IP — too late to filter by original port |

Lower than -200 breaks conntrack. Higher than -100 misses Docker traffic.

### Other chains

**INPUT** mirrors the gate's port rules as defense-in-depth — if something bypasses prerouting (kernel bug, misconfiguration), INPUT still protects host services. Uses the same named sets, so no config duplication.

**FORWARD** is `policy accept` — Docker manages forwarding in its own `table ip docker-bridges`. The gate already filtered unauthorized traffic before it reaches FORWARD.

**Docker native nftables** (`daemon.json: {"firewall-backend": "nftables"}`) — Docker creates its own table separate from ours. No iptables translation layer, no rule conflicts.

## Packet Flow

### External → allowed port (host or container)

```
packet(:443) → gate → port in allowed_tcp? YES → accept
  → routing: host process listening? → INPUT (accept, defense-in-depth)
  → routing: Docker DNAT match?     → container receives it
```

### External → port not allowed

```
packet(:9999) → gate → port in allowed_tcp? NO → policy drop
  (Docker DNAT never sees this packet)
```

### Docker -p but port not in fw allow

```
docker run -p 8080:80 app
packet(:8080) → gate → 8080 not in allowed_tcp → DROP
  (Docker added DNAT rule, but gate blocked it first)
```

This is the core value — Docker can publish whatever it wants, nothing gets through without `fw allow`. Both `fw allow` and Docker `-p` are required for external container access.

### Container → internet (outbound)

```
container → br-* interface → gate: trusted iface → ACCEPT → NAT → internet
```

`iifname "br-*"` trusts all Docker bridge interfaces by wildcard. Docker creates `br-<hash>` per network — enumerating them would be fragile. Trade-off: a compromised container has unrestricted outbound access.

### Blocked IP

```
packet(src=1.2.3.4) → gate → established? NO → blocked_ips? YES → DROP
```

Blocked IP check runs after established — existing connections continue until they close, new connections are dropped. The `blocked_ips` set is shared by `fw block-ip` and fail2ban (which auto-bans after 3 SSH failures in 5min → 1h ban, escalating to 30 day via recidive jail).

## Gate Chain — Rule Order

```
1. iifname @trusted_ifaces accept     ← lo, docker0 bypass everything
2. iifname "br-*" accept              ← Docker bridges bypass (wildcard)
3. ct state invalid drop               ← malformed packets
4. ct state established,related accept ← return traffic for outbound
5. ip saddr @blocked_ips drop          ← fail2ban + manual bans
6. meta l4proto icmp,icmpv6 accept     ← ping, path MTU discovery
7. tcp dport @allowed_tcp accept       ← the port whitelist
8. udp dport @allowed_udp accept
9. limit rate 5/s reject               ← polite error for legit clients
10. drop                                ← silent drop (anti-DDoS)
```

**Why this order:**
- Trusted interfaces first → zero overhead for internal traffic
- Conntrack before IP check → don't break existing connections mid-flow
- IP block before port check → banned IPs can't reach even allowed ports
- ICMP before ports → ping always works (needed for path MTU discovery)
- Rate-limited reject before final drop → legitimate clients get fast errors, but attackers can't use ICMP flood for amplification

Ports are stored in nftables named sets — O(1) lookup and atomic add/remove. No ruleset reload needed, no momentary gap in filtering.

## Invariants

1. **Never `nft flush ruleset`** — destroys Docker's `table ip docker-bridges`
2. **Only manage `table inet firewall`** — Docker's tables are Docker's problem
3. **`fw allow` + Docker `-p` are both required** for external container access
4. **`fw save` captures current state** — including transient fail2ban bans
5. **fail2ban restart clears ban state** — IPs re-banned only on next trigger
