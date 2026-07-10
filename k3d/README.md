# k3d on this host — DNS & networking runbook

Operational notes for running **k3d / k3s** on this Arch box, which has an unusual combination:

- Kernel 7.1+, Arch
- **Docker 29 with `firewall-backend: nftables`** (experimental — required because the custom firewall in [`../firewall`](../firewall) depends on it)
- Custom nftables firewall, default `policy drop`
- k3d (with `K3D_FIX_DNS` enabled by default)

This combination breaks **in-cluster DNS in four independent ways**. This file records the symptoms, root causes, and the verified recipe so you don't have to re-diagnose from scratch.

---

## TL;DR — Create the cluster correctly

```bash
# 1. A real resolv.conf (host's NextDNS here; 8.8.8.8 also works but bypasses filtering)
printf 'nameserver 45.90.28.169\nnameserver 45.90.30.169\n' | sudo tee /etc/k3d-resolv.conf

# 2. Create cluster: disable k3d's DNS fix + pin resolv.conf + masquerade-all
K3D_FIX_DNS=false k3d cluster create main \
  --volume '/etc/k3d-resolv.conf:/etc/resolv.conf@server:*' \
  --k3s-arg '--kube-proxy-arg=masquerade-all=true@server:*'
```

**Prerequisite:** the firewall must permit DNS from the Docker bridges to the host — two rules in the `input` chain of [`../firewall/nftables.conf`](../firewall/nftables.conf). This is *not* a public port-53 opening (external 53 is still dropped by the gate); it only lets containers reach the host's embedded resolver. It ships as part of the firewall config, so no k3d-specific step is needed — just make sure the current `nftables.conf` is loaded.

> ⚠️ `K3D_FIX_DNS=false` must be in the **environment of the shell running k3d**, NOT passed as `--env` to the node.
> If you don't disable it, k3d fails with: `Cannot activate DNS fix (K3D_FIX_DNS) when there's a file mounted at /etc/resolv.conf!`

---

## The 4 problems & why those 3 flags exist

| # | Symptom | Root cause | Fix |
|---|---|---|---|
| 1 | `Host is unreachable`, ping works but DNS dead | Firewall (`input` chain, `policy drop`) rejects UDP/53 from the bridge to the embedded DNS. ICMP is accepted, so ping still works → misleading | DNS rules in `firewall/nftables.conf` |
| 2 | `Connection refused` when the node queries DNS | `K3D_FIX_DNS` (on by default) rewrites resolv.conf to `nameserver 172.19.0.1` (the gateway), assuming a resolver lives there. Under the Docker **nftables backend** the gateway is **empty** (the embedded resolver only listens on `127.0.0.11`) → dead address | `K3D_FIX_DNS=false` + pinned resolv.conf |
| 3 | CoreDNS `CrashLoopBackOff`, `[FATAL] plugin/loop` | If the node uses `127.0.0.11`: CoreDNS (own netns) forwards to itself → loop → self-terminates | Pin a **real** DNS IP (routable in every netns) → no loop |
| 4 | Pod DNS via ClusterIP `10.43.0.10` times out | Hairpin: single-node cluster → client pod & CoreDNS share bridge cni0. The reply goes straight over L2 (src = real pod IP, not the ClusterIP) → conntrack can't reverse the DNAT → client drops it | `--kube-proxy-arg=masquerade-all=true` |

**Core insight:** `127.0.0.11` is a loopback address — only valid inside **one** netns. A k3d node contains many pods (each its own netns), so DNS must be a **real, routable IP that works everywhere** → that's why you pin it instead of letting Docker/k3d choose.

Note on `masquerade-all`: it SNATs all traffic to services, so services see the node IP instead of the original pod IP. Harmless for a dev cluster; only matters if you use NetworkPolicy based on source IP.

---

## Quick diagnostics (when it breaks again)

```bash
NODE=k3d-main-server-0   # adjust to your cluster name

# 0. Can the node resolve? (this is what containerd uses to pull images)
docker exec $NODE nslookup registry-1.docker.io

# 1. Where does the node's resolv.conf point? (must be a REAL IP, NOT 172.19.0.1 or 127.0.0.11)
docker exec $NODE cat /etc/resolv.conf

# 2. Is the firewall blocking DNS? (ping OK but nslookup 'Host is unreachable' = firewall)
docker exec $NODE ping -c1 172.19.0.1        # usually OK (ICMP)
docker exec $NODE nslookup x.io 172.19.0.1   # 'Host is unreachable' = firewall dropping 53

# 3. CoreDNS
docker exec $NODE kubectl -n kube-system logs deploy/coredns --tail=20 | grep -i loop

# 4. Hairpin: compare ClusterIP vs direct pod IP
CIP=$(docker exec $NODE kubectl -n kube-system get pod -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}')
docker exec $NODE kubectl run t --image=busybox:1.36 --restart=Never --rm -i --command -- nslookup x.io $CIP        # pod IP: should work
docker exec $NODE kubectl run t --image=busybox:1.36 --restart=Never --rm -i --command -- nslookup x.io 10.43.0.10  # ClusterIP: if FAIL → missing masquerade-all
```

### Tested evidence (2026-07-10)

| Config (firewall already fixed) | resolv.conf | Node pull | CoreDNS | Hairpin ClusterIP |
|---|---|---|---|---|
| No DNS pin | `172.19.0.1` ❌ | Connection refused | — | — |
| DNS pinned correctly | NextDNS ✅ | ✅ | ✅ Running | ❌ timeout |
| DNS pinned + `masquerade-all` | NextDNS ✅ | ✅ | ✅ | ✅ |

---

## Why NOT to revert Docker to the iptables backend

The iptables backend is mature and doesn't have bug #2, but the custom firewall ([`../firewall`](../firewall)) is **designed specifically for the nftables backend** — a single "gate" at `prerouting`, assuming Docker manages its own separate nftables tables. Forcing Docker back to the iptables backend would make Docker's iptables rules collide with your firewall. So the right path is **keep the nftables backend + patch the 4 issues above**, not switch backends.

---

## References

- [Docker docs — nftables backend (experimental, Docker 29.0.0)](https://docs.docker.com/engine/network/firewall-nftables/) — self-described experimental; partial DNS support; doesn't auto-enable ip_forward; no `DOCKER-USER` equivalent
- [k3d #1515 — K3D_FIX_DNS=1 breaks external DNS](https://github.com/k3d-io/k3d/issues/1515) — workaround `K3D_FIX_DNS=0`
- [k3d #1516 — DNS not working on pods](https://github.com/k3d-io/k3d/issues/1516)
- [k3d #209 — DNS not resolving (origin of K3D_FIX_DNS)](https://github.com/k3d-io/k3d/issues/209)
- [k3s #9921 — Pods can't resolve DNS](https://github.com/k3s-io/k3s/issues/9921)
- [k3s Advanced docs — resolv.conf loopback check & `--resolv-conf`](https://docs.k3s.io/advanced)
- [TrueNAS — Kubernetes DNS broken, masquerade fix](https://www.truenas.com/community/threads/kubernetes-dns-resolution-broken.115186/)
