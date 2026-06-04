{{/*
cloudflared (Cloudflare Tunnel) sidecar — included into the pod spec by
deployment.yaml when .Values.tunnel.enabled (the renderer sets that ONLY for a
prod app with a public route). Token mode: `cloudflared tunnel run` reads
TUNNEL_TOKEN from the runtime Secret <app>-runtime (populated for prod by the
ExternalSecret from SSM, key TUNNEL_TOKEN under the app's SSM root) and serves the
app's public hostnames through the Tunnel to its OWN ClusterIP Service.

This is exposure with NO LoadBalancer and NO Ingress object — the Tunnel IS the
ingress, so the platform's no-LB guardrail holds. With a token tunnel the
hostname->service ingress + public DNS are configured once in Cloudflare when the
tunnel is created (NOTES.txt documents the `cloudflared tunnel route dns` step);
.Values.tunnel.ingress records the intended mapping as platform metadata.
*/}}
{{- define "app.cloudflaredSidecar" -}}
- name: cloudflared
  image: {{ .Values.tunnel.image | default "cloudflare/cloudflared:2024.10.0" }}
  imagePullPolicy: IfNotPresent
  args: ["tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"]
  env:
    - name: TUNNEL_TOKEN
      valueFrom:
        secretKeyRef:
          name: {{ include "app.secretName" . }}
          key: TUNNEL_TOKEN
  ports:
    - name: cf-metrics
      containerPort: 2000
      protocol: TCP
  livenessProbe:
    httpGet:
      path: /ready
      port: cf-metrics
    initialDelaySeconds: 10
    periodSeconds: 15
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 100m
      memory: 128Mi
{{- end -}}
