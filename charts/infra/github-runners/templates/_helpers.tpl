{{/*
Expand the name of the chart.
*/}}
{{- define "github-runners.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Chart name + version label, with Flux's "+<sha>" build metadata stripped so
helm.sh/chart stays STABLE across commits (reconcileStrategy: Revision appends it).
*/}}
{{- define "github-runners.chart" -}}
{{- printf "%s-%s" .Chart.Name (.Chart.Version | toString | splitList "+" | first) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every rendered RunnerDeployment / HRA (k8s metadata
labels — distinct from the GitHub runner `labels` in values).
*/}}
{{- define "github-runners.labels" -}}
helm.sh/chart: {{ include "github-runners.chart" . }}
app.kubernetes.io/name: {{ include "github-runners.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: ci-runners
platform/managed-by: platformctl
{{- end }}
