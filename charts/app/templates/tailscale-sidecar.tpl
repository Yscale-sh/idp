{{/*
Tailscale EGRESS sidecar — included into the pod spec by deployment.yaml when
.Values.tailscale.enabled (the renderer sets that ONLY for a non-local/prod app
that declared `tailscaleEgress: true`). Userspace tailscaled joins the tailnet so
the app can reach out-of-cluster services that have no in-cluster DNS — e.g. the
on-prem Loki for log shipping from a cloud provider prod cluster.

Mirrors the pattern the apps already ran with on managed Kubernetes: userspace mode (no NET_ADMIN,
no /dev/net/tun), ephemeral node (TS_KUBE_SECRET=""), TS_AUTHKEY pulled from the
runtime Secret <app>-runtime (populated for prod by the ExternalSecret from the
SHARED key /shared/tailscale/auth-key). No host privileges, so it holds on the
hardened Kubernetes nodes.
*/}}
{{- define "app.tailscaleSidecar" -}}
- name: tailscale
  image: {{ .Values.tailscale.image | default "ghcr.io/tailscale/tailscale:v1.78.1" }}
  imagePullPolicy: IfNotPresent
  env:
    - name: TS_AUTHKEY
      valueFrom:
        secretKeyRef:
          name: {{ include "app.secretName" . }}
          key: TS_AUTHKEY
    - name: TS_USERSPACE
      value: "true"
    - name: TS_KUBE_SECRET
      value: ""
    - name: TS_AUTH_ONCE
      value: "true"
    - name: TS_HOSTNAME
      value: {{ .Values.tailscale.hostname | quote }}
    - name: TS_STATE_DIR
      value: /tmp/tailscale
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    allowPrivilegeEscalation: false
    capabilities:
      drop: ["ALL"]
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 64Mi
{{- end -}}
