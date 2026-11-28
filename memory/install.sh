#!/bin/bash
# Installs the memory settings in memory/etc. Run: sudo bash memory/install.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }

(cd "$here/etc" && find . -type f) | while read -r f; do
    install -D -m 0644 "$here/etc/$f" "/etc/${f#./}"
    echo "installed /etc/${f#./}"
done

sysctl -p /etc/sysctl.d/99-swappiness.conf /etc/sysctl.d/99-watermark.conf
systemd-tmpfiles --create /etc/tmpfiles.d/thp-madvise.conf
systemctl daemon-reload
systemctl restart earlyoom.service

# zram-generator applies a new zram-size at the next boot. Restarting
# systemd-zram-setup@zram0 now would swapoff zram and pull every swapped page
# back into RAM.
echo "zram0: $(swapon --show=SIZE --noheadings /dev/zram0 2>/dev/null || echo missing) (a new zram-size applies at the next boot)"

if swapon --show=NAME,TYPE --noheadings | awk '$1 !~ /^\/dev\/zram/' | grep -q .; then
    echo "WARNING: disk swap is active; this setup expects zram only:" >&2
    swapon --show >&2
fi
