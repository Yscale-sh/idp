# Diagrams — current vs target, and the deploy module

Mermaid (renders on GitHub / most markdown editors). ASCII versions of #2 and #3 are in the chat.

## 1. BEFORE — today (~$235/mo): 2 clusters, 7 NodeBalancers, managed Postgres
```mermaid
flowchart TB
  Net["Internet"]
  subgraph PROD["prod_cluster · LKE us-east · 3 nodes"]
    NB1["NB $10<br/>cartogopher"]:::nb --> CG["cartogopher + openprophet<br/>(nginx proxy)"]
    NB2["NB $10<br/>dummy-api"]:::nb --> DA["dummy-api"]
    NB3["NB $10<br/>ecommerce"]:::nb --> EC["ecommerce"]
    NB4["NB $10<br/>unbelievable"]:::nb --> UN["unbelievable"]
  end
  subgraph ANY["anyrent · LKE us-ord · 4 nodes"]
    NB5["NB $10<br/>anyrent-api"]:::nb --> AA["anyrent-api"]
    NB6["NB $10<br/>anyrent-ui"]:::nb --> AU["anyrent-ui"]
    NB7["NB $10<br/>metabase (dead)"]:::nb --> MB["metabase"]
  end
  PGM[("Managed Postgres<br/>$32/mo")]
  GoUS[("GoUS Nanode<br/>ecommerce DB")]
  Net --> NB1 & NB2 & NB3 & NB4 & NB5 & NB6 & NB7
  AA --> PGM
  EC -. "Tailscale" .-> GoUS
  classDef nb fill:#ffe0e0,stroke:#cc0000,color:#000;
```

## 2. AFTER — target (~$50–72/mo recurring): 1 k3s cluster, Cloudflare Tunnel, self-hosted PG
```mermaid
flowchart TB
  USERS["Users / Services"]
  subgraph CF["Cloudflare (free)"]
    DNS["DNS + edge TLS + DDoS"]
    ACC["Access<br/>SSO + service tokens"]
    TUN["Tunnel"]
  end
  USERS --> DNS --> TUN
  ACC -. "guards" .-> TUN
  subgraph K["ONE k3s cluster · Linode"]
    CFD["cloudflared"]
    subgraph H["Minimal host — always-on, never autoscaled"]
      PG[("Postgres<br/>StatefulSet")]
      EXTRA["pgbouncer · redis<br/>core svcs"]
    end
    subgraph M["yscale mesh — elastic, scales with load"]
      APPS["anyrent-api/ui · cartogopher · openprophet<br/>ecommerce · dummy-api · unbelievable · carshowdb"]
      JOBS["batch jobs / scrapers"]
    end
  end
  TUN --> CFD --> APPS
  APPS --> EXTRA --> PG
  PG -. "WAL + base backups" .-> R2[("Cloudflare R2")]
  classDef ok fill:#e0ffe0,stroke:#00a000,color:#000;
  class TUN,PG ok;
```

## 3. The deploy module — submit a YAML → runner clones module → deploys
```mermaid
flowchart LR
  DEV["Developer"] -->|"deploy.yaml + git push"| APP["App repo<br/>code + deploy.yaml + ship.yml"]
  MOD[("platform MODULE @v1<br/>app-chart + deploy.sh + schema")]
  APP -->|"calls reusable workflow @v1"| R
  subgraph R["Self-hosted runner (in-cluster)"]
    direction TB
    B["build & push<br/>stackmaster/app:prod-sha"] --> C["clone module @v1"] --> V["deploy.sh<br/>validate vs schema"] --> HU["helm upgrade app-chart<br/>(ClusterIP — LoadBalancer forbidden)"]
  end
  MOD -. "cloned by" .-> C
  HU --> CLU[("k3s cluster")]
  V --> TR["ensure Tunnel route + DNS"]
  V --> ST["mint Access service token → SSM"]
```
