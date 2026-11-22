# rebootd

rebootd reboots the host when it receives a signed UDP packet on port 39017. It is for the case where SSH is unreachable but the kernel still runs.

On 2026-11-22 every glibc name lookup on beanbocchi-internal hung from 07:50 to 17:52. sshd sessions, cron jobs and caddy all waited on those lookups, so nobody could log in, and RAM was 34% free the whole time. rebootd is built with `CGO_ENABLED=0` and never calls glibc NSS.

## Protocol

The client and the daemon share a 32-byte key. The daemon reads it from `/etc/rebootd/key`, and the client reads it from `~/.config/remote-reboot/key`.

Request, one UDP packet:

```
v1 <action> <unix-seconds> <nonce> <hmac>
hmac = HMAC-SHA256(key, "v1 <action> <unix-seconds> <nonce>")
```

The daemon drops the packet without a reply when any check fails:

| Check | Rejects |
|---|---|
| Five fields, `v1`, known action, 32-hex nonce | malformed packets and port scans |
| HMAC matches | anyone without the key |
| Timestamp within 60 s of the host clock | old captured packets |
| Nonce not seen in the last 120 s | a captured packet replayed inside the 60 s window |

Reply, one UDP packet:

```
<text> <hmac>
hmac = HMAC-SHA256(key, "<nonce> <text>")
```

The client checks the reply HMAC, so a spoofed reply is reported as invalid. The text contains uptime, load, `MemAvailable`, `SwapFree` and the memory PSI `full avg60`, all read from `/proc`.

## Actions

| Action | What the daemon does |
|---|---|
| `ping` | Replies with the status text. |
| `reboot` | sysrq `s`, then `systemctl reboot`. If the daemon is still running 90 s later, sysrq `s`, `u`, `b`. |
| `reboot-now` | sysrq `s`, `u`, `b`, about 5 s in total. |

During a normal shutdown systemd sends SIGTERM to rebootd and the daemon exits, which cancels the 90 s fallback. The fallback runs only when systemd never gets that far. A reboot runs at most once per daemon process.

sysrq `s` syncs the disks, `u` remounts every filesystem read-only and `b` reboots at once. A root write to `/proc/sysrq-trigger` works whatever `kernel.sysrq` is set to.

## Running under memory pressure

| Setting | Effect |
|---|---|
| `mlockall(MCL_CURRENT\|MCL_FUTURE)` with `LimitMEMLOCK=infinity` | the daemon's pages stay in RAM while the host swaps |
| `OOMScoreAdjust=-1000` | the kernel OOM killer and earlyoom never pick it |
| `MemoryMin=64M` | the cgroup's memory is protected from reclaim |
| `/proc/sysrq-trigger` opened at startup | the reboot path needs no new file open |
| Log lines go through a 256-entry channel and are dropped when it is full | a stuck journald cannot block the packet loop |

## Install

On the host:

```bash
sudo bash rebootd/install.sh
rebootd/client/remote-reboot --host 127.0.0.1 ping
```

`install.sh` builds the binary as the calling user, installs it to `/usr/local/bin/rebootd`, creates `/etc/rebootd/key` if it is missing, copies the key to `~/.config/remote-reboot/key`, enables `rebootd.service` and runs `fw allow 39017/udp -l rebootd`. Run `sudo fw save` afterwards to keep the port open across reboots; `fw save` also writes the current fail2ban bans into `/etc/nftables.conf`.

On the router, forward UDP 39017 to the host.

On the laptop, while SSH still works:

```bash
scp <host>:linux-server-setup/rebootd/client/remote-reboot ~/.local/bin/
mkdir -p ~/.config/remote-reboot
scp <host>:.config/remote-reboot/key ~/.config/remote-reboot/key
chmod 600 ~/.config/remote-reboot/key
```

## Usage

```bash
remote-reboot ping
remote-reboot reboot
remote-reboot reboot-now
remote-reboot --host 203.0.113.7 reboot   # when the DDNS name points at an old IP
```

The client sends up to three tries, each with a new nonce, and waits 3 s for each reply. `REMOTE_REBOOT_HOST` and `REMOTE_REBOOT_PORT` set the defaults for `--host` and `--port`. The client and the host clocks must agree within 60 s.

## Limits

rebootd needs a running kernel and a network path to the host. A frozen kernel needs the hardware watchdog (`iTCO_wdt` on this host, enabled with `RuntimeWatchdogSec=` in systemd). A stale DDNS name needs `--host` with the current IP.

## Files

| File | Installs to |
|---|---|
| `cmd/rebootd/main.go` | built to `/usr/local/bin/rebootd` |
| `rebootd.service` | `/etc/systemd/system/rebootd.service` |
| `client/remote-reboot` | the laptop's `PATH` |
| `install.sh` | not installed |
