#!/bin/bash
# Builds and installs rebootd. Run from your own account: sudo bash rebootd/install.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
port=39017

[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
owner=${SUDO_USER:?run with sudo from a normal account, not as root}

# The build runs as the calling user, so Go's cache stays in that user's home.
sudo -u "$owner" env CGO_ENABLED=0 go -C "$here" build -trimpath -ldflags="-s -w" -o rebootd ./cmd/rebootd

install -m 0755 "$here/rebootd" /usr/local/bin/rebootd
install -m 0644 "$here/rebootd.service" /etc/systemd/system/rebootd.service

# The key is created only when /etc/rebootd/key is missing or empty.
install -d -m 0700 /etc/rebootd
if [[ ! -s /etc/rebootd/key ]]; then
    (umask 077; od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > /etc/rebootd/key)
fi

# A copy for the local client, so the setup can be tested on this host.
home=$(getent passwd "$owner" | cut -d: -f6)
install -d -m 0700 -o "$owner" -g "$owner" "$home/.config/remote-reboot"
install -m 0600 -o "$owner" -g "$owner" /etc/rebootd/key "$home/.config/remote-reboot/key"

systemctl daemon-reload
systemctl enable rebootd.service
systemctl restart rebootd.service
systemctl --no-pager status rebootd.service | head -5

# fw allow changes the live ruleset only. fw save also writes fail2ban's
# current bans into /etc/nftables.conf, so it is left to the operator.
if command -v fw >/dev/null; then
    fw allow "$port/udp" -l rebootd
    echo "Run 'sudo fw save' to keep $port/udp open after a reboot."
fi
