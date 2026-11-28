#!/bin/bash
# Installs the hardware watchdog settings. Run: sudo bash watchdog/install.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }

install -D -m 0644 "$here/etc/modprobe.d/90-watchdog.conf" /etc/modprobe.d/90-watchdog.conf
install -D -m 0644 "$here/etc/systemd/system.conf.d/90-watchdog.conf" /etc/systemd/system.conf.d/90-watchdog.conf

# The settings take effect at the next boot, when intel_oc_wdt is not loaded
# and iTCO_wdt is /dev/watchdog0.
for w in /sys/class/watchdog/watchdog*; do
    echo "$(basename "$w"): $(cat "$w/identity") state=$(cat "$w/state")"
done
echo "After the next boot, watchdog0 should be iTCO_wdt with state=active, and"
echo "'systemctl show -p RuntimeWatchdogUSec' should print RuntimeWatchdogUSec=1min."
