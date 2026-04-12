# fw Hierarchical Labels Design

Replaces the flat `-c "comment"` system with structured `group.child` labels.

## Syntax

```bash
# Grouped: -g sets group, :child on each arg
sudo fw allow -g shopnexus 6379:redis 5432:postgres 8082:api

# Flat label (no group)
sudo fw allow 22 -l SSH

# Group membership without child label
sudo fw allow 3000 -g myproject

# Retroactive labeling (dot notation)
sudo fw label 6379 shopnexus.redis
sudo fw label 22 SSH

# IPs work the same
sudo fw block-ip -g scanners 1.2.3.4:april-12 5.6.7.8:april-12
```

## Sidecar File

`/etc/fw-labels.conf` — replaces `/etc/fw-comments.conf`. Simple `key=value`:

```
6379/tcp=shopnexus.redis
5432/tcp=shopnexus.postgres
22/tcp=SSH
1.2.3.4=scanners.april-12
```

Migration: on first run, if `fw-comments.conf` exists and `fw-labels.conf` does not, rename it.

## `fw list` Output

```
=== Allowed TCP Ports ===
  shopnexus:
    6379      redis
    5432      postgres
    8082      api
  22          SSH
  8000        (no label)

=== Blocked IPs ===
  scanners:
    1.2.3.4   april-12
    5.6.7.8   april-12
```

- Grouped entries: group header, then indented children
- Flat labels: inline after the port/IP
- Unlabeled: `(no label)` inline

## Argument Parsing Rules

- `-l <label>` — flat label, no dots, applies to all args in the call
- `-g <group>` — group name, args use `:child` suffix for child labels
- `-l` and `-g` are mutually exclusive
- One of `-l` or `-g` is required (every entry must be labeled)
- Args without `:child` under `-g` get just the group name (no child)
- `fw label <port[/proto]|ip> <label>` — retroactive, uses dot notation for grouped

## Cleanup

- `fw block` / `fw allow-ip` removes the entry's label from the sidecar
- `fw label` overwrites any existing label for that key

## Files Changed

- `firewall/fw` — all changes in this single file
- `/etc/fw-labels.conf` — new sidecar (replaces `/etc/fw-comments.conf`)
