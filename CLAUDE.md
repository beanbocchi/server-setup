# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Repo Is

A collection of Linux server configuration modules. Each subdirectory is a self-contained setup for a specific system concern (firewall, etc.), targeting **Arch Linux** hosts running **Docker**.

## Architecture

### firewall/ — Unified nftables Firewall with Docker Native Integration

A single **gate chain** at the `prerouting` hook (priority `-150`) filters ALL incoming external traffic — both host services and Docker containers — through one shared set of allowed ports. See `firewall/README.md` for the full architecture diagram and rule-by-rule walkthrough.

Key design decisions:
- **Gate at prerouting priority -150**: after conntrack (-200) so `ct state` works, before Docker DNAT (-100) so original destination ports are visible. Priority -400 or lower breaks conntrack.
- **`policy drop` (fail-closed)**: if rules are flushed, all external traffic is blocked.
- **Docker uses native nftables** (`"firewall-backend": "nftables"` in daemon.json) — Docker manages its own `table ip docker-bridges`, completely separate from `table inet firewall`.
- **`destroy table` for idempotent reload**: `nftables.conf` starts with `destroy table inet firewall` which deletes and recreates only our table. This is the safe reload pattern — never use `nft flush ruleset` which destroys Docker's tables too.
- Bridge interfaces (`br-*`) are trusted by wildcard; other trusted interfaces use a named set.

### fail2ban — Automatic IP Banning

fail2ban monitors sshd logs and adds offending IPs directly to the existing `blocked_ips` nft set using a custom action. No new chains or sets needed — bans appear in `fw list` automatically.

- **sshd jail**: 1h ban after 3 failures in 5min
- **recidive jail**: 30 day ban after 3 jail hits in 12h (escalation for repeat offenders)

### Files and install locations

| File | Installs to | Requires |
|------|------------|----------|
| `firewall/nftables.conf` | `/etc/nftables.conf` | root |
| `firewall/daemon.json` | `/etc/docker/daemon.json` | Docker restart |
| `firewall/fw` | `/usr/local/bin/fw` | chmod 755 |
| `firewall/fail2ban/jail.local` | `/etc/fail2ban/jail.local` | fail2ban installed |
| `firewall/fail2ban/action.d/nftables-blocked.conf` | `/etc/fail2ban/action.d/nftables-blocked.conf` | fail2ban installed |

## Common Commands

```bash
# Apply firewall changes after editing nftables.conf
sudo cp firewall/nftables.conf /etc/nftables.conf
sudo nft -f /etc/nftables.conf

# Manage ports (live, immediate effect)
sudo fw allow 80
sudo fw block 80
sudo fw allow 53/udp
sudo fw list

# Persist live state to config (survives reboot)
sudo fw save

# Verify firewall is working
sudo nft list chain inet firewall gate
sudo nft list set inet firewall allowed_tcp

# Debug: watch packets hitting the gate in real-time
sudo nft monitor trace

# Test if a port is reachable
echo "quit" | nc -w 2 <host-ip> <port>; echo $?

# Check Docker's tables (never modify these manually)
sudo nft list table ip docker-bridges
```

## Conventions

- All firewall configs use `inet` family (dual IPv4/IPv6).
- The `fw` script is a bash wrapper around `nft` commands — changes to set management logic go there.
- Docker port publishing (`-p`) creates DNAT rules automatically, but the gate still blocks the port unless it's in `allowed_tcp`/`allowed_udp`. Both `fw allow` AND `-p` are needed for external access to containers.

```bash
# fail2ban status
sudo fail2ban-client status sshd
sudo fail2ban-client status recidive

# Manually unban
sudo fail2ban-client set sshd unbanip 1.2.3.4
```

## Safety Rules

- **Never `nft flush ruleset`** — destroys Docker's `table ip docker-bridges`. Only manage `table inet firewall` via `destroy table` or `nft delete table inet firewall`.
- **`fw save` writes live kernel state to `/etc/nftables.conf`** — this overwrites the installed config, not the repo copy. Make sure the firewall table is in the desired state before saving. If fail2ban has active bans, those IPs get persisted too.
- **fail2ban restart clears its ban state** — on restart, fail2ban unbans all tracked IPs (removes them from `blocked_ips`). They'll be re-banned if they trigger the jail again.
- **Restarting Docker stops all containers** — plan accordingly when changing `daemon.json`.

## Caveats

- Docker `network_mode: host` containers use the host's network stack — their ports are controlled by the gate like any host service.
- macvlan/ipvlan Docker networks don't use `br-*` interfaces — add their interfaces to `trusted_ifaces` if needed.
- Container outbound traffic from bridge interfaces bypasses the gate (trusted). A compromised container can make arbitrary outbound connections.
