# Firewall Setup — nftables + Docker Native

A unified firewall for Linux hosts running Docker. Uses a single **gate chain** at the `prerouting` hook to filter ALL incoming traffic — both host services and Docker containers — through one set of allowed ports.

## Architecture

```
External packet arrives
  |
  v
+------------------------------------------+
| GATE chain (prerouting, priority -150)   |  <-- single choke point
|                                          |
| Trusted interface? (lo, docker, bridge)  |---> ACCEPT (internal traffic)
|                                          |
| Established connection?                  |---> ACCEPT (response traffic)
| Blocked IP?                              |---> DROP
| Allowed port?                            |---> ACCEPT
| Everything else                          |---> REJECT/DROP
+------------------+-----------------------+
                   |
                   v (only allowed traffic continues)
            Routing decision
            |                |
            v                v
     +------+------+  +-----+------+
     | INPUT chain |  | Docker     |
     | (host)      |  | DNAT +     |
     | policy drop |  | FORWARD    |
     | (backup)    |  | (container)|
     +-------------+  +------------+
```

### Priority Chain in the Prerouting Hook

```
Priority  What                          Purpose
--------  ----------------------------  ----------------------------------
-300      Docker raw-PREROUTING         DROP DIRECT ACCESS to container IPs
-200      Conntrack                     Track connections (ct state works after this)
-150      ** GATE (our chain) **        Filter by port — sees original dest port
-100      Docker nat-PREROUTING (DNAT)  Rewrite dest port → container IP
   0      filter (INPUT/FORWARD)        Defense-in-depth
```

The gate runs AFTER conntrack (so `ct state established` works) but BEFORE Docker's DNAT (so it sees the original destination port, not the rewritten container port).

## Files

| File | Install location | Purpose |
|------|-----------------|---------|
| `nftables.conf` | `/etc/nftables.conf` | Firewall rules |
| `daemon.json` | `/etc/docker/daemon.json` | Docker native nftables backend |
| `fw` | `/usr/local/bin/fw` | CLI tool to manage ports |

## Installation

```bash
# 1. Install the fw CLI tool
sudo cp fw /usr/local/bin/fw
sudo chmod 755 /usr/local/bin/fw

# 2. Install and load firewall rules
sudo cp nftables.conf /etc/nftables.conf
sudo systemctl enable nftables
sudo systemctl start nftables

# 3. Configure Docker to use native nftables
sudo cp daemon.json /etc/docker/daemon.json
sudo systemctl restart docker
```

> **Warning**: Restarting Docker stops all running containers. Plan accordingly.

## Usage — `fw` CLI

```bash
# Open/close TCP ports (default protocol)
sudo fw allow 80
sudo fw allow 443
sudo fw block 80

# UDP ports
sudo fw allow 53/udp
sudo fw block 53/udp

# Port ranges
sudo fw allow 8000-9000

# Block/unblock IP addresses
sudo fw block-ip 1.2.3.4
sudo fw block-ip 10.0.0.0/8
sudo fw allow-ip 1.2.3.4

# View current state
sudo fw list          # show allowed ports and blocked IPs
sudo fw status        # full nftables table dump

# Persistence
sudo fw save          # write current live state to /etc/nftables.conf
sudo fw reload        # reload rules from /etc/nftables.conf
```

### Key behavior

- `fw allow <port>` opens the port for **both** host services and Docker containers.
- `fw block <port>` closes the port for **everything**.
- Changes are **live immediately** but lost on reboot unless you run `fw save`.
- `fw save` dumps the current kernel state to `/etc/nftables.conf`. The `nftables` systemd service loads this on boot.

## How It Works

### Named Sets

Ports are stored in nftables **named sets** — hash tables in the kernel. Adding or removing a port is O(1) and atomic. No ruleset reload needed.

```
set allowed_tcp = { 22, 80, 443 }     # managed by: fw allow / fw block
set allowed_udp = { 53 }              # managed by: fw allow 53/udp
set blocked_ips = { 1.2.3.4 }         # managed by: fw block-ip / fw allow-ip
set trusted_ifaces = { "lo", "docker0" }  # add VPN interfaces here
```

### Gate Chain — Rule by Rule

```
chain gate {
    type filter hook prerouting priority -150; policy drop;

    # 1. Trusted interfaces bypass all filtering
    #    - lo: localhost traffic
    #    - docker0, br-*: container internal traffic
    #    - Add VPN interfaces (wg0, tailscale0) to trusted_ifaces set
    iifname @trusted_ifaces accept
    iifname "br-*" accept

    # 2. Drop malformed packets (conntrack couldn't classify)
    ct state invalid drop

    # 3. Allow responses to connections we initiated
    #    Without this, outbound curl/apt/etc would break
    ct state { established, related } accept

    # 4. Drop banned IPs before checking ports
    ip saddr @blocked_ips drop

    # 5. Allow ICMP (ping, traceroute, path MTU discovery)
    meta l4proto { icmp, icmpv6 } accept

    # 6. Allow traffic to permitted ports
    tcp dport @allowed_tcp accept
    udp dport @allowed_udp accept

    # 7. Rate-limited reject (5/sec), then silent drop
    #    Prevents DDoS amplification while giving fast errors to legit clients
    limit rate 5/second reject with icmpx admin-prohibited
    drop
}
```

### INPUT Chain — Defense in Depth

Mirrors the gate chain's port filtering. If something bypasses the gate (kernel bug, misconfiguration), INPUT still protects the host. Uses the same `allowed_tcp`/`allowed_udp` sets.

### FORWARD Chain — Docker Managed

`policy accept` — Docker's own `table ip docker-bridges` handles per-bridge forwarding, published port allow-listing, and inter-container communication. Our gate already filtered unauthorized traffic before it reaches FORWARD.

## Docker Integration

### daemon.json

```json
{
    "firewall-backend": "nftables"
}
```

This switches Docker from the legacy `iptables-nft` translation layer to **native nftables**. Docker creates its own table (`table ip docker-bridges`) with:

- **DNAT rules**: map published ports (`-p 8080:80`) to container IPs
- **Verdict maps**: O(1) per-bridge chain dispatch (replaces linear iptables chain jumping)
- **Per-bridge forwarding**: only published ports are allowed in, containers can talk to each other (ICC) and reach the internet
- **MASQUERADE**: rewrites container source IPs to host IP for outbound traffic
- **DROP DIRECT ACCESS**: prevents external access to container IPs directly (must go through DNAT)

### How Docker Port Publishing Works With the Gate

```
1. You run: docker run -p 8888:3000 myapp
   Docker adds DNAT rule: tcp dport 8888 → 172.22.0.5:3000

2. External request to host:8888
   Gate: port 8888 not in allowed_tcp → REJECTED

3. You run: sudo fw allow 8888
   Adds 8888 to allowed_tcp set

4. External request to host:8888
   Gate: port 8888 in allowed_tcp → ACCEPT
   Docker DNAT: rewrite dest to 172.22.0.5:3000
   Docker forward: published port → ACCEPT
   Container receives request on port 3000
```

Publishing a port in Docker is **not enough** — the gate must also allow it. This is the intended security model: Docker handles routing, the gate handles access control.

### Kernel Table Layout

```
sudo nft list tables

table ip docker-bridges     # Docker IPv4 (auto-managed)
table ip6 docker-bridges    # Docker IPv6 (auto-managed)
table inet firewall         # Your firewall (managed by fw)
```

Never run `nft flush ruleset` — it destroys Docker's tables. Only manage your own table:
- `nft -f /etc/nftables.conf` (reload your table)
- `nft delete table inet firewall` (remove only your table)

## Adding Trusted Interfaces

For VPN or custom interfaces that should bypass the gate:

```bash
# WireGuard
sudo nft add element inet firewall trusted_ifaces { "wg0" }

# Tailscale
sudo nft add element inet firewall trusted_ifaces { "tailscale0" }

# Persist
sudo fw save
```

Docker bridge interfaces (`br-*`) are matched by wildcard and don't need to be added to the set.

## Security Model

| Traffic type | Filtered by | Default |
|---|---|---|
| External → host port | Gate + INPUT | **Blocked** (must `fw allow`) |
| External → container published port | Gate | **Blocked** (must `fw allow`) |
| Container → container (same network) | Docker ICC rules | Allowed |
| Container → internet | Not filtered by gate | Allowed |
| Host → internet | Not filtered by gate | Allowed |
| Localhost → anything | Trusted interface | Allowed |

### Design Decisions

- **Fail-closed**: gate chain `policy drop` — if rules are flushed, all external traffic is blocked.
- **Rate-limited reject**: first 5 blocked packets/sec get an ICMP error (fast client feedback), the rest are silently dropped (prevents DDoS amplification).
- **Single port set**: one `fw allow` command opens a port for both host and containers. Simplicity over granularity.
- **Defense in depth**: INPUT chain duplicates the gate's port filtering as a backup layer.

## Troubleshooting

```bash
# See what's allowed right now
sudo fw list

# Full ruleset dump
sudo fw status

# Watch packets hitting the gate in real-time
sudo nft monitor trace

# Check if a specific port is open
echo "quit" | nc -w 2 <host-ip> <port>; echo $?
# exit 0 = connected, exit 1 = blocked

# Something broke? Reload from saved config
sudo fw reload

# Nuclear option: remove firewall entirely (Docker keeps working)
sudo nft delete table inet firewall
```

## Caveats

- **Docker `network_mode: host`** containers use the host's network stack directly. Their ports are controlled by the gate like any host service.
- **macvlan/ipvlan** Docker networks don't use `br-*` interfaces and won't match the bridge wildcard. Add their interfaces to `trusted_ifaces` if needed.
- **Container outbound traffic** from bridge interfaces bypasses the gate (marked as trusted). A compromised container can make arbitrary outbound connections. For production, consider adding outbound filtering.
- **`fw save` overwrites** `/etc/nftables.conf` with the live kernel state. If Docker is not running when you save, Docker's tables won't be affected (they're in separate tables), but make sure your firewall table is in the desired state before saving.

## fail2ban Integration

fail2ban monitors service logs and automatically bans offending IPs by adding them to the shared `blocked_ips` nft set — the same set used by `fw block-ip` and checked by the gate chain.

### Files

| File | Install location | Purpose |
|------|-----------------|---------|
| `fail2ban/jail.local` | `/etc/fail2ban/jail.local` | Jail definitions |
| `fail2ban/action.d/nftables-blocked.conf` | `/etc/fail2ban/action.d/nftables-blocked.conf` | Custom action to manipulate `blocked_ips` |

### Installation

```bash
# 1. Install fail2ban
sudo pacman -S fail2ban

# 2. Install config files
sudo cp fail2ban/jail.local /etc/fail2ban/jail.local
sudo mkdir -p /etc/fail2ban/action.d
sudo cp fail2ban/action.d/nftables-blocked.conf /etc/fail2ban/action.d/nftables-blocked.conf

# 3. Enable and start
sudo systemctl enable --now fail2ban
```

### Jails

**sshd** — brute-force protection:
- 3 failed login attempts in 5 minutes → 1 hour ban
- Uses `systemd` backend (reads from journalctl)

**recidive** — repeat offender escalation:
- 3 bans from any jail within 12 hours → 30 day ban
- Monitors `/var/log/fail2ban.log`

### How It Works

```
1. Attacker fails SSH login 3 times in 5 minutes
2. fail2ban detects this in the journal
3. fail2ban runs: nft add element inet firewall blocked_ips { <attacker-ip> }
4. Gate chain drops all traffic from that IP immediately
5. After 1 hour, fail2ban runs: nft delete element inet firewall blocked_ips { <attacker-ip> }

If the same IP gets banned 3 times in 12 hours:
6. Recidive jail bans them for 30 days
```

Banned IPs appear in `fw list` under "Blocked IPs" alongside manually blocked IPs. For fail2ban-specific info:

```bash
# Check jail status
sudo fail2ban-client status sshd
sudo fail2ban-client status recidive

# Manually unban an IP
sudo fail2ban-client set sshd unbanip 1.2.3.4
```

### Caveats

- **`fw save` captures fail2ban bans** — if you run `fw save` while IPs are banned, those IPs will be persisted in `nftables.conf` and survive reboots even after fail2ban would have unbanned them. Run `fw save` when the ban list is clean, or accept that some IPs may stay blocked longer than intended.
- **fail2ban restart clears its internal state** — on restart, fail2ban unbans all IPs it was tracking (removing them from `blocked_ips`). Previously banned IPs can reconnect until they trigger the jail again.
- **Recidive uses file backend** — it reads `/var/log/fail2ban.log`, not the journal. If log rotation deletes that file, recidive loses its history.
