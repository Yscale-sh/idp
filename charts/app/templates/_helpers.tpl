{{/*
charts/app helpers.

NAMING: per the platformctl contract one app == one namespace == one release ==
one service, all named <app>. The canonical handle is .Values.platform.app
(rendered by platformctl from appconfig App.App). We fall back to .Release.Name
so the chart still renders sanely under `helm template <name> charts/app` with a
bare values file.
*/}}

{{/* The workload handle: platform.workload (<app>-<component> for a multi-component
app), else platform.app, else the release name. This names the Deployment/Service/
Secret so sibling components of one app never collide. */}}
{{- define "app.name" -}}
{{- $h := .Values.platform.workload | default .Values.platform.app -}}
{{- default .Release.Name $h | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fullname == name. We intentionally do NOT prefix the release name; the
platform guarantees release == app, so <app> is already the fully-qualified
namespaced name. */}}
{{- define "app.fullname" -}}
{{- include "app.name" . }}
{{- end }}

{{/* Chart label value, e.g. app-0.1.0. The version is stripped of any build
metadata (the "+<sha>" Flux appends under reconcileStrategy: Revision) so
helm.sh/chart stays STABLE across commits — otherwise the pod template label
churns and every app rolls on every unrelated platform-repo commit. */}}
{{- define "app.chart" -}}
{{- printf "%s-%s" .Chart.Name (.Chart.Version | toString | splitList "+" | first) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels — the immutable subset matched by the Deployment/Service. These
must NEVER change for a given release (they are part of the Deployment selector),
so they are deliberately limited to the two app.kubernetes.io identity labels.
*/}}
{{- define "app.selectorLabels" -}}
app.kubernetes.io/name: {{ include "app.name" . }}
app.kubernetes.io/instance: {{ include "app.name" . }}
{{- end }}

{{/*
Common labels — stamped on every generated object. This is the full platformctl
label set (app.kubernetes.io/* + platform/*) plus Helm bookkeeping labels.
platform/* values come from .Values.platform (rendered from App.Labels(env)).
*/}}
{{- define "app.labels" -}}
helm.sh/chart: {{ include "app.chart" . }}
{{ include "app.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.platform.app }}
platform/app: {{ . | quote }}
{{- end }}
{{- with .Values.platform.env }}
platform/env: {{ . | quote }}
{{- end }}
platform/managed-by: {{ default "platformctl" .Values.platform.managedBy | quote }}
{{- with .Values.platform.product }}
platform/product: {{ . | quote }}
{{- end }}
{{- with .Values.platform.component }}
platform/component: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
The runtime Secret name materialized by the ExternalSecret: <app>-runtime.
*/}}
{{- define "app.secretName" -}}
{{- printf "%s-runtime" (include "app.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
app.image builds "<repository>:<tag>".

The real guardrails live in the platformctl policy layer (Go), which runs BEFORE
any render: it rejects an empty/mutable ("latest") tag in prod and requires a
fully-qualified --image from CI. The chart is env-agnostic, so it must stay
render-safe for `helm lint`/`helm template` with a bare values file. We therefore
do NOT `fail` here; instead an unset repository/tag falls back to an obviously
invalid placeholder that could never be pulled, surfacing the mistake loudly
without breaking lint/template. The renderer always supplies both.
*/}}
{{- define "app.image" -}}
{{- $repo := .Values.image.repository | default "ghcr.io/yscale-sh/UNSET" -}}
{{- $tag := .Values.image.tag | default "UNSET" -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end }}

{{/*
serviceTypeGuard is the hard ClusterIP guardrail. The Service template hardcodes
ClusterIP, but in case a values file (or a future edit) ever introduces a
service.type that is not ClusterIP, we fail the render. A LoadBalancer can never
be produced by this chart.
*/}}
{{- define "app.serviceTypeGuard" -}}
{{- $t := .Values.service.type | default "ClusterIP" -}}
{{- if ne $t "ClusterIP" -}}
{{- fail (printf "service.type must be ClusterIP (apps are ClusterIP only; got %q). LoadBalancer is forbidden by platform policy." $t) -}}
{{- end -}}
{{- end }}
