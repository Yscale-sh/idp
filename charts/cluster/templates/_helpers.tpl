{{/*
Common Flux HelmRelease bits shared by the app, store, and module templates.
*/}}

{{/* Source ref (the GitRepository the bootstrap provides) for in-repo charts. */}}
{{- define "cluster.gitSourceRef" -}}
kind: GitRepository
name: {{ .Values.source.name }}
namespace: {{ .Values.source.namespace }}
{{- end -}}

{{/* install/upgrade remediation block reused by store + module inner HelmReleases. */}}
{{- define "cluster.remediation" -}}
install:
  createNamespace: true
  remediation:
    retries: 3
upgrade:
  remediation:
    retries: 3
{{- end -}}

{{/* App-release remediation. NON-PROD disables the Helm readiness wait: an app
component that is externally KEDA-scaled and/or GPU-constrained never reports
"fully available" (a desired replica stays Pending on GPU capacity), so a waiting
Helm upgrade times out and Flux ROLLS BACK a perfectly good image — which reverted
the yscale-media-transcode eac3 fix in a loop (2026-06-19). disableWait makes the
upgrade fix-forward instead. Prod keeps the strict wait + rollback. Arg: dict "env" $env */}}
{{- define "cluster.appRemediation" -}}
install:
  createNamespace: true
  {{- if ne .env "prod" }}
  disableWait: true
  {{- end }}
  remediation:
    retries: 3
upgrade:
  {{- if ne .env "prod" }}
  disableWait: true
  {{- end }}
  remediation:
    retries: 3
{{- end -}}

{{/* Platform labels for an app's inner releases. Arg: dict "ctx" $ "name" .name "component" .component */}}
{{- define "cluster.appLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .name }}
platform/app: {{ .app }}
platform/env: {{ .env }}
platform/component: {{ .component }}
platform/managed-by: platformctl
{{- end -}}
