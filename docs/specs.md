# Firewall Specs

## Why

Docker modifies iptables/nftables directly, bypassing traditional INPUT chain firewalls. Published ports (`-p 8080:80`) become globally reachable even with UFW/iptables rules blocking them — because Docker's DNAT rules run before INPUT.

This firewall solves that by placing a **single gate at prerouting** — before Docker's DNAT — so every external packet must pass through our allowed-port check first. One choke point blocks everything by default, including Docker ports.

## Architecture

```
                    External packet
                         │
                         ▼
              ┌─────────────────────┐
              │  conntrack (-200)   │  Track connection state
              └────────┬────────────┘
                       │
                       ▼
              ┌─────────────────────┐
              │   ★ GATE (-150)     │  OUR CHAIN — single choke point
              │                     │
              │  trusted iface? ──────► ACCEPT (lo, br-*, docker0)
              │  established?   ──────► ACCEPT
              │  blocked IP?    ──────► DROP
              │  allowed port?  ──────► ACCEPT
              │  everything else ─────► DROP (policy)
              └────────┬────────────┘
                       │ only accepted traffic
                       ▼
              ┌─────────────────────┐
              │  Docker DNAT (-100) │  Rewrites dest → container IP
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

This is the core value — Docker can publish whatever it wants, nothing gets through without `fw allow`.

### Container → internet (outbound)

```
container → br-* interface → gate: trusted iface → ACCEPT → NAT → internet
```

Bridge interfaces are trusted. Outbound is unrestricted.

### Blocked IP

```
packet(src=1.2.3.4) → gate → established? NO → blocked_ips? YES → DROP
```

Blocked IP check runs after established check — existing connections from a banned IP continue until they close. New connections are dropped.

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
- Rate-limited reject before final drop → legitimate clients get fast errors, but attackers can't amplify via ICMP flood

## Design Decisions

### Single gate at prerouting (not INPUT)

**Problem:** Docker's DNAT at priority -100 rewrites the destination before INPUT sees the packet. A traditional INPUT-only firewall cannot control Docker-published ports.

**Solution:** Filter at prerouting priority -150. Every external packet — host or container — hits one gate, one set of allowed ports.

### policy drop (fail-closed)

If rules get flushed or the gate chain breaks, the default is DROP everything. No silent fail-open.

### Named sets (not chain rules)

Ports are stored in nftables named sets (`allowed_tcp`, `allowed_udp`). Adding/removing a port is O(1) and atomic — no ruleset reload, no momentary gap in filtering.

### Bridge wildcard trust

`iifname "br-*"` trusts all Docker bridge interfaces by wildcard. Docker creates `br-<hash>` per network — enumerating them would be fragile and break on network create/destroy.

**Trade-off:** A compromised container on a bridge network has unrestricted outbound access. Acceptable for non-hostile workloads.

### INPUT as defense-in-depth

The INPUT chain mirrors the gate's port rules as a backup. If something bypasses prerouting (kernel bug, misconfiguration), INPUT still protects host services. Uses the same sets, so no config duplication.

### FORWARD policy accept

Docker manages forwarding in its own `table ip docker-bridges`. Our gate already filtered unauthorized traffic — FORWARD doesn't need to re-check.

### Docker native nftables backend

`daemon.json: {"firewall-backend": "nftables"}` — Docker creates its own table (`table ip docker-bridges`) separate from ours. No iptables translation layer, no rule conflicts.

### Rate-limited reject + silent drop

First 5 rejected packets/second get ICMP admin-prohibited (fast feedback for legitimate clients hitting the wrong port). Beyond that, silent drop — prevents DDoS amplification via ICMP flood.

### Dual-protocol default

`fw allow 80` opens both TCP and UDP. Simpler mental model — one command, one port, done. Use `fw allow 80/tcp` to be specific.

## Components

### nftables.conf

Declares `table inet firewall` with four chains (gate, input, forward, output) and four named sets (allowed_tcp, allowed_udp, blocked_ips, trusted_ifaces). Loaded by `systemctl start nftables`.

Starts with `destroy table inet firewall` for idempotent reload — drops and recreates only our table. Never `flush ruleset` (destroys Docker's table).

### fw CLI

Bash wrapper around `nft` commands. Manages set elements (allow/block ports and IPs), labels, and persistence.

**Labels:** Stored in `/etc/fw-labels.conf` as `key=value`. Supports flat labels (`SSH`) and hierarchical groups (`shopnexus.redis`). Labels are display-only — not used in filtering logic.

**Save:** Dumps live kernel state to `/etc/nftables.conf`. This includes fail2ban bans if active.

### fail2ban

Monitors sshd journal. Adds banned IPs to the existing `blocked_ips` set via a custom action — no new chains or tables.

- **sshd jail:** 3 failures in 5min → 1h ban
- **recidive jail:** 3 bans in 12h → 30 day ban

Bans appear in `fw list` alongside manual blocks.

## Invariants

1. **Never `nft flush ruleset`** — destroys Docker's `table ip docker-bridges`
2. **Only manage `table inet firewall`** — Docker's tables are Docker's problem
3. **`fw allow` + Docker `-p` are both required** for external container access
4. **`fw save` captures current state** — including transient fail2ban bans
5. **fail2ban restart clears ban state** — IPs re-banned only on next trigger
