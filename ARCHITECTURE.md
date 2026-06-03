# Target architecture: minimal host + yscale mesh + Cloudflare Tunnel — NO ACTION YET

> **Still 100% Kubernetes.** "Minimal host" = a small **k3s** node (full k8s API), not a move off
> Kubernetes. Everything stays k8s-native: apps are `Deployment`+`Service` via the IDP Helm chart,
> Postgres is a `StatefulSet`+Service, yscale is a dynamically-scaling k8s node mesh, cloudflared is
> a `Deployment`. Savings come from fewer/leaner nodes + Tunnel + self-hosted PG — not from dropping k8s.

The cheapest *and* most scalable shape on the table. Replaces the "one fat LKE cluster" node layer
in `MIGRATION.md §3`. Everything else in `MIGRATION.md` (Cloudflare Tunnel ingress, self-hosted
Postgres per `POSTGRES_BACKUPS.md`, the IDP app-chart, namespace model) stays the same.

```
                 Cloudflare  (DNS + edge TLS + DDoS + free Tunnel)
                                  │
                     ┌────────────┴─────────────┐
                     │   cloudflared (in-cluster) │   all ingress, $0 NodeBalancers
                     └────────────┬─────────────┘
   ┌─────────────────────────────┴──────────────────────────────┐
   │  MINIMAL HOST  (k3s on Linode, always-on, never scaled away) │
   │  • k3s control plane                                          │
   │  • Postgres (+ pgbouncer, redis)  ← all stateful lives here   │
   │  • always-on core services + cloudflared                      │
   │  • the cheap floor: 1 small Linode                            │
   └───────────────────────────────────────────────────────────────┘
                                  │  KEDA signals load / schedule / prediction
                     ┌────────────┴─────────────┐
                     │          yscale           │  ← stateless apps + jobs run here
                     │  dynamically-scaling k8s  │     availability in any DC as needed
                     │  mesh; KEDA-native        │     (elastic — pay while serving)
                     └───────────────────────────┘
```

## The two parts

### 1. Minimal host (the floor)
A single small always-on k3s node on Linode that runs the control plane plus everything stateful and
always-on (Postgres, redis, pgbouncer, cloudflared, core services). This is the only guaranteed cost.
Lean by design — one node when nothing's happening.

### 2. yscale — the dynamic scaling layer
> **A dynamically-scaling Kubernetes mesh that puts availability in any datacenter as needed and
> works with KEDA.**

Stateless apps and batch jobs run on the mesh; it expands and contracts with demand, so capacity
only costs money while it's serving load. The minimal host stays put. Treated as a black-box
capability here — the internals are yscale's job.

### 3. Cloudflare Tunnel (ingress)
All public traffic enters through `cloudflared`, so there are **no NodeBalancers and no public IPs**
on any node — including the elastic mesh nodes, which come and go. New app = a Tunnel route + DNS
CNAME. (You already run this pattern in carshowdb.)

---

## The one hard rule: stateful stays on the minimal host
The mesh is elastic — nodes appear and vanish. **Postgres, redis, and anything with a PVC must run
on the minimal host, never on the mesh**, or an automatic scale-down can take your database with it.
The IDP app-chart enforces this: stateful workloads pin to the host (`nodeSelector: role=baseline`);
stateless apps and jobs are free to schedule onto the mesh. KEDA drives the mesh; the host is fixed.

## Two things to decide / accept
- **Control-plane resilience.** A single self-hosted k3s control plane is a SPOF (running pods
  survive an API outage, but nothing reschedules until it's back). Mitigate cheaply with regular k3s
  datastore snapshots → R2; or run 3-server embedded-etcd k3s for true HA (raises the floor). Pick
  the trade you want. (LKE's free managed control plane is the alternative if you'd rather not own
  this — but it pins you to one region and the CCM, which the mesh model is designed to escape.)
- **Storage.** k3s doesn't bundle the Linode CSI; install it so Postgres PVCs survive node loss
  (don't use local-path for the DB). Backups per `POSTGRES_BACKUPS.md`.

---

## Measured usage — why the floor can be tiny (evidence)
Pulled from anyrent's in-cluster Prometheus (2026-06-01, 7-day window):

| Resource | Allocatable (4 nodes) | Requested | Used now | **7-day MAX** |
|---|---|---|---|---|
| CPU | 4.00 cores | 2.79 | 0.25 | **0.26 cores** |
| Memory | 7.3 GiB | 4.1 | 2.7 | **2.8 GiB** |

The anyrent cluster is **~6% CPU / ~38% memory utilized**, and 7-day peak is essentially flat —
it's idle. Top consumers are Prometheus + calico (system overhead), not the apps (anyrent-api
0.09 GiB, anyrent-ui 0.08 GiB, documenso 0.24 GiB). Most of the 2.8 GiB is **per-node system pods
×4**; consolidating to one node removes ~3 nodes' worth of that overhead.

**Sizing conclusion:** the whole anyrent workload fits in **one 4 GB node** (with headroom for
Postgres). CPU is a non-constraint; size the host for memory (~3 GiB working set). prod_cluster
has no Prometheus, so its history is unknown — get a `kubectl top` snapshot before sizing it in, but
its apps (small Go/static services) are unlikely to change the picture.

> Caveat: 7-day max of 0.26 cores means there's been no real load this week. yscale's elastic mesh
> is exactly what covers genuine traffic spikes, so the *baseline* can stay this small safely.

## Cost shape

| Line | Today | Minimal host + yscale mesh |
|---|---:|---:|
| Baseline compute (1 minimal host) | $84 (7 nodes) | ~$12–24 |
| Peak/job capacity (yscale, on-demand) | — | pay-as-scaled (~$5–15 typical) |
| NodeBalancers | $70 | **$0** (Cloudflare Tunnel) |
| Postgres | $32 managed | ~$4 self-host + R2 |
| Orphaned volumes | $6 | $0 |
| GPU/ephemeral burst | ~$14 | ~$14 (+ cost cap) |
| Object storage | $5 | $5 |
| Misc/tax | ~$24 | ~$8 |
| **Total** | **~$235** | **~$50–70/mo** |

The bill is mostly the minimal host + Postgres now; everything else is elastic and only costs money
while it's actually serving load. The two levers doing the work: **Cloudflare Tunnel (−$70)** and
**lean floor + dynamic mesh (−$60+)**.

## How this changes the migration
`MIGRATION.md` already gets you off NodeBalancers and off managed Postgres. This swaps its Phase 3–4
("migrate everything onto one fat LKE cluster, right-size to ~4 nodes") for **"stand up the minimal
k3s host + yscale mesh, move apps onto it via the IDP chart, point Cloudflare Tunnel at it, retire
both LKE clusters."** Same destination (one production environment, IDP-driven, Tunnel ingress),
leaner floor, and it runs on your own stack. Phases 1–2 (Tunnel cutover, self-host Postgres) are
still worth doing first since they bank ~$100/mo independently of the cluster move.
