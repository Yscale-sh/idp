{{/*
Common Flux HelmRelease bits shared by the app, store, and module templates.
*/}}

{{/* Source ref (the GitRepository the bootstrap provides) for in-repo charts. */}}
{{- define "cluster.gitSourceRef" -}}
kind: GitRepository
name: {{ .Values.source.name }}
namespace: {{ .Values.source.namespace }}
{{- end -}}

{{/* install/upgrade remediation block reused by every inner HelmRelease. */}}
{{- define "cluster.remediation" -}}
install:
  createNamespace: true
  remediation:
    retries: 3
upgrade:
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
