# watchdog

systemd pings the hardware watchdog every 30 s. When the kernel or PID 1 hangs and 60 s pass without a ping, the hardware resets the machine. [rebootd](../rebootd) covers a host whose kernel still runs; this module covers a frozen kernel.

## Devices on beanbocchi-internal

| Driver | Device without the blacklist | Notes |
|---|---|---|
| `intel_oc_wdt` | `/dev/watchdog0` | Intel overclocking watchdog |
| `iTCO_wdt` | `/dev/watchdog1` | Intel PCH TCO timer, version 4 on this board |

systemd opens `/dev/watchdog0` unless `WatchdogDevice=` says otherwise, and the numbering follows driver load order. `etc/modprobe.d/90-watchdog.conf` blacklists `intel_oc_wdt`, so `iTCO_wdt` is the only watchdog and becomes `/dev/watchdog0`.

## Settings

`etc/systemd/system.conf.d/90-watchdog.conf`:

| Setting | Value | Effect |
|---|---|---|
| `RuntimeWatchdogSec` | `60s` | reset after 60 s without a ping from PID 1 |
| `RebootWatchdogSec` | `10min` | reset when a reboot has not finished after 10 minutes |

## Install

```bash
sudo bash watchdog/install.sh
```

The settings take effect at the next boot. After it:

```bash
cat /sys/class/watchdog/watchdog0/identity   # iTCO_wdt
cat /sys/class/watchdog/watchdog0/state      # active
systemctl show -p RuntimeWatchdogUSec        # RuntimeWatchdogUSec=1min
```

Nobody has tested whether the reset fires on this board, because a test needs a deliberate kernel hang.
