# Matching managed-Postgres backup level on self-hosted (NO ACTION YET)

## What we're matching
Akamai/Linode Managed PostgreSQL gives:
- **Automatic daily backups**, **~14-day retention** (7–14 restore points)
- **Point-in-time recovery (PITR)** — restore to any moment in the window, one-click
- Off-host backup storage, automatic security/maintenance updates
- (single-node caveat: it takes **downtime during maintenance**)

Your current self-host plan = **daily `pg_dump → R2`**. That's a fine *extra* safety net, but it
is **not** parity: RPO is up to 24h and there is **no PITR**. To match managed you need continuous
WAL archiving + periodic base backups + retention, tied together by a tool that can do PITR.

## The gap, precisely
| Capability | Managed PG | `pg_dump` nightly (current sketch) | Need |
|---|---|---|---|
| Backup cadence | continuous WAL + daily base | once/day logical dump | continuous WAL + base |
| RPO (max data loss) | seconds–minutes | up to 24h | seconds–minutes |
| PITR (restore to a timestamp) | ✅ | ❌ | ✅ |
| Retention | ~14 days | configurable | 14 days |
| Restore effort | one click | manual `gunzip \| psql` | automated |
| Off-host storage | ✅ | ✅ (R2) | ✅ (R2) |

## Recommendation: CloudNativePG operator (managed-parity on k8s)
CloudNativePG natively does **continuous physical backup + WAL archiving to S3-compatible storage
(R2), PITR, retention policies, scheduled base backups, compression + encryption** — and it manages
**primary + replicas with automatic failover**, which also satisfies your "multiple pods each" ask
*and* beats the managed single-node on maintenance downtime. You already have the R2 bucket +
credentials (`r2-credentials` / `anyrentcloudprod`), so storage is solved.

> CloudNativePG uses Barman Cloud under the hood. On v1.26+ the object-store config moved to the
> `barman-cloud` plugin; on ≤1.25 it's the in-tree `barmanObjectStore` shown below. Same result.

### Cluster + backup config (sketch)
```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: anyrent-pg, namespace: platform }
spec:
  instances: 2                       # primary + replica → HA + read scaling (your "multiple pods")
  storage:
    size: 40Gi
    storageClass: linode-block-storage   # Linode CSI; survives node loss (NOT local-path)
  postgresql:
    parameters:
      max_slot_wal_keep_size: "2GB"  # cap WAL growth — the exact cause of both April disk incidents
  backup:
    retentionPolicy: "14d"           # match managed's 14-day window
    barmanObjectStore:
      destinationPath: s3://anyrentcloudprod/pg/anyrent-pg
      endpointURL: https://<r2-account>.r2.cloudflarestorage.com
      s3Credentials:
        accessKeyId:     { name: r2-credentials, key: ACCESS_KEY_ID }
        secretAccessKey: { name: r2-credentials, key: SECRET_ACCESS_KEY }
      wal:  { compression: gzip, maxParallel: 2 }   # continuous WAL archiving → PITR
      data: { compression: gzip }                    # base backups
---
apiVersion: postgresql.cnpg.io/v1
kind: ScheduledBackup
metadata: { name: anyrent-pg-daily, namespace: platform }
spec:
  schedule: "0 3 * * *"              # daily base backup at 03:00 UTC (like managed)
  cluster: { name: anyrent-pg }
  backupOwnerReference: self
```

### Point-in-time restore (the "one-click" equivalent)
Spin up a new Cluster that bootstraps from the object store and stops at a target time:
```yaml
spec:
  bootstrap:
    recovery:
      source: anyrent-pg
      recoveryTarget: { targetTime: "2026-06-01 14:35:00+00" }
  externalClusters:
    - name: anyrent-pg
      barmanObjectStore:
        destinationPath: s3://anyrentcloudprod/pg/anyrent-pg
        endpointURL: https://<r2-account>.r2.cloudflarestorage.com
        s3Credentials: { accessKeyId: {...}, secretAccessKey: {...} }
```
That replays WAL to 14:35:00 — true PITR, matching managed.

## Belt-and-suspenders: keep the `pg_dump` too
Physical PITR (above) protects against host/disk loss and gives a tight RPO. **Also keep your daily
`pg_dump → R2`** because logical dumps protect against things physical backups don't: single-table
restores, logical corruption, and cross-major-version restores. So the final posture is:
- **CloudNativePG**: continuous WAL + daily base + 14d retention + PITR  ← matches/exceeds managed
- **pg_dump cronjob** (existing): nightly logical safety net  ← keep as-is

## Restore drills (the part managed hides from you)
Self-hosting means *you* own restore confidence. Add a monthly automated job that restores the
latest backup into a throwaway namespace and runs a row-count/checksum sanity check — so "we have
backups" is actually "we have *verified-restorable* backups." Managed implicitly does this; do it explicitly.

## Cost
R2 has **no egress fees** and ~$0.015/GB-mo. For a ~2GB DB: compressed base backups + 14 days of WAL
≈ a few GB → **well under $1/mo**, on top of the ~$4/mo Linode block volume for the data PVC. Still
**~$28/mo cheaper** than the $32/mo managed tier — now with equal-or-better recovery.

## Lighter alternative (if you don't want an operator)
Run the StatefulSet you already have + add **WAL-G** (or Barman) as a sidecar for WAL archiving +
weekly base backup to R2. Gets you PITR without CloudNativePG's CRDs, but you hand-manage failover
and retention. CloudNativePG is recommended because it automates failover + retention + restore the
way managed did.

## Sources
- [Akamai Managed Databases — daily backups, 14-day retention, PITR](https://www.akamai.com/products/databases) · [Aiven-backed clusters](https://techdocs.akamai.com/cloud-computing/docs/aiven-database-clusters)
- [CloudNativePG — Backup & PITR to S3/R2](https://cloudnative-pg.io/documentation/current/backup/) · [Recovery](https://cloudnative-pg.io/documentation/current/recovery/)
