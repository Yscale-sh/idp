# CloudNativePG-compatible Postgres 15 image: postgis (carshowdb) + pgvector (anyrent).
# Derived from the official CNPG postgis operand so the instance manager, barman-cloud
# (R2 PITR), and entrypoint contract stay intact — we only ADD the pgvector extension.
FROM ghcr.io/cloudnative-pg/postgis:15-3.3

USER root
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends postgresql-15-pgvector; \
    rm -rf /var/lib/apt/lists/*
# CNPG operand runs as uid 26 (postgres)
USER 26
