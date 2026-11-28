# memory

Swap, reclaim and OOM settings for beanbocchi-internal, an Arch host with 32 GB of RAM that runs Docker, a k3d cluster and services for several users. Swap is zram only, and earlyoom kills a process before the kernel runs out of memory.

## Settings

| File | Setting | Reason |
|---|---|---|
| `etc/systemd/zram-generator.conf` | `zram0`, `zram-size = ram`, zstd, priority 100 | Swap compressed in RAM. On 2026-11-28 zram held 115 MB in 31 MB, a ratio of 3.8. |
| `etc/sysctl.d/99-swappiness.conf` | `vm.swappiness=150`, `vm.page-cluster=0` | With zram, swapping anonymous pages costs less than dropping page cache. `page-cluster=0` reads one page per swap-in, which suits zram. |
| `etc/sysctl.d/99-watermark.conf` | `vm.watermark_scale_factor=125`, `vm.watermark_boost_factor=0` | kswapd starts reclaiming at about 400 MB above the min watermark instead of about 30 MB, so fewer processes stall in direct reclaim. |
| `etc/default/earlyoom` | `-m 4,2 -s 100,100`, `--prefer`, `--avoid` | SIGTERM at 4% available memory (about 1.25 GB), SIGKILL at 2%, whatever the swap level. |
| `etc/tmpfiles.d/thp-madvise.conf` | THP `madvise` | Transparent huge pages only for programs that ask for them. |

earlyoom prefers `text-embeddings`, `text-embeddings-router` and `llama-server`, and avoids `init`, `systemd`, `Xorg`, `sshd`, `dockerd`, `containerd`, `k3s`, `caddy`, `cli-proxy-api` and `headscale`. `java` is in neither list, because both the Minecraft server and a Kafka broker run as `java`.

## History

On 2026-11-17 the host made 50 global OOM kills in 20 minutes with 24 GB of swap still free. `vm.swappiness` was 10, so the kernel dropped page cache to about 7 MB instead of swapping to zram. earlyoom never fired, because its default needs swap below 10%. The kernel kept killing best-effort pods (`oom_score_adj` 1000) instead of the pod that held 5.5 GB. `99-swappiness.conf`, the earlyoom arguments and the THP setting date from that day.

On 2026-11-28 the RAM went from 19 GB to 32 GB. The 16 GB `/swapfile` on the NVMe drive was removed: it was used only after zram filled, when the host was already thrashing. `99-watermark.conf` dates from that day.

## Install

```bash
sudo bash memory/install.sh
```

The script copies `etc/` to `/etc`, applies both sysctl files, applies the THP setting and restarts earlyoom. A change to `zram-generator.conf` applies at the next boot. The script prints a warning when disk swap is active.

## Checks

```bash
swapon --show                      # only /dev/zram0
cat /proc/pressure/memory          # "full avg60" above about 20% means thrashing
journalctl -u earlyoom -n 5        # hourly memory report, and any kills
```

`/proc/pressure/memory` measures memory trouble better than used RAM does. Used RAM is high on a healthy host because of page cache and zram.
