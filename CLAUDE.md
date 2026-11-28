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

### k3d/ — Runbook for k3d/k3s DNS on this host

Not a config module — a troubleshooting runbook. The combination on this host (Docker 29 **nftables backend** + the custom nftables firewall + k3d's default `K3D_FIX_DNS`) breaks in-cluster DNS in four independent ways. `k3d/README.md` has the verified cluster-create recipe and per-symptom diagnostics.

Key facts:
- **Firewall must allow DNS from Docker bridges.** The `input` chain in `firewall/nftables.conf` includes two rules permitting tcp/udp 53 from `docker0`/`br-*` to the host. Without them, container→embedded-DNS is rejected (ICMP still passes, so ping misleadingly works).
- **`K3D_FIX_DNS` (on by default) is harmful here.** It rewrites the node resolv.conf to the bridge gateway (`172.19.0.1`), which has no resolver under the nftables backend. Create clusters with `K3D_FIX_DNS=false` (in the shell env, not `--env`) and pin a real resolv.conf via `--volume`.
- **Single-node ClusterIP needs hairpin masquerade.** Add `--k3s-arg '--kube-proxy-arg=masquerade-all=true@server:*'` or same-node pod→ClusterIP→pod (incl. CoreDNS `10.43.0.10`) times out.
- **Do not revert Docker to the iptables backend** — the firewall design depends on the nftables backend.

### rebootd/ — Remote reboot over signed UDP

A Go daemon on UDP 39017 that reboots the host on an HMAC-signed request, for when SSH is unreachable but the kernel runs. `rebootd/README.md` has the protocol, the reboot sequence and the install steps.

Key facts:
- Built with `CGO_ENABLED=0`, so it never calls glibc NSS. On 2026-11-22 every glibc name lookup on this host hung for ten hours, and sshd hung with it.
- `reboot` runs `systemctl reboot` and falls back to sysrq `s`, `u`, `b` after 90 s. SIGTERM from a normal shutdown ends the daemon and cancels the fallback.
- The port has to be open in two places: `fw allow 39017/udp` on the host (done by `install.sh`, persisted by `fw save`) and a UDP forward on the router.
- Invalid packets get no reply. Keep it that way, or a port scan can find the daemon.

### memory/ — Swap and OOM settings

Swap is zram only (`zram-size = ram`, zstd); the 16 GB `/swapfile` was removed on 2026-11-28 when the host went to 32 GB. `memory/README.md` lists every setting with the incident that produced it.

Key facts:
- `vm.swappiness=150` and `vm.page-cluster=0` suit zram. The 2026-11-17 OOM livelock happened at `swappiness=10`, with 24 GB of swap free.
- earlyoom runs with `-s 100,100`, so it acts on available memory alone. Its default needs swap below 10% and never fired on 2026-11-17.
- Do not add `java` to earlyoom's `--prefer`: the Minecraft server and a Kafka broker both run as `java`.
- Judge memory trouble by `/proc/pressure/memory` (`full avg60`), not by used RAM.

### watchdog/ — Hardware watchdog

`RuntimeWatchdogSec=60s` on `iTCO_wdt`. `intel_oc_wdt` is blacklisted because it loads first and takes `/dev/watchdog0`, the device systemd opens by default; both settings apply at the next boot.

### Files and install locations

| File | Installs to | Requires |
|------|------------|----------|
| `firewall/nftables.conf` | `/etc/nftables.conf` | root |
| `firewall/daemon.json` | `/etc/docker/daemon.json` | Docker restart |
| `firewall/fw` | `/usr/local/bin/fw` | chmod 755 |
| `firewall/fail2ban/jail.local` | `/etc/fail2ban/jail.local` | fail2ban installed |
| `firewall/fail2ban/action.d/nftables-blocked.conf` | `/etc/fail2ban/action.d/nftables-blocked.conf` | fail2ban installed |
| `rebootd/cmd/rebootd/` | `/usr/local/bin/rebootd` | Go, `sudo bash rebootd/install.sh` |
| `rebootd/rebootd.service` | `/etc/systemd/system/rebootd.service` | installed by `install.sh` |
| key generated by `install.sh` | `/etc/rebootd/key` and `~/.config/remote-reboot/key` | not in the repo |
| `memory/etc/**` | the same path under `/etc` | `sudo bash memory/install.sh`; zram changes at next boot |
| `watchdog/etc/**` | the same path under `/etc` | `sudo bash watchdog/install.sh`; applies at next boot |

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
