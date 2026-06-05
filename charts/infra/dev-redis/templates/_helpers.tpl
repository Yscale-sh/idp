{{/* Expand the name of the chart. */}}
{{- define "dev-redis.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified name: <release>-<chart> unless the release already contains it
(matches clusterenv.DevRedisService: <app>-redis-dev-redis). */}}
{{- define "dev-redis.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Chart label (version stripped of Flux "+<sha>" build metadata so it stays
STABLE across commits — same reasoning as dev-postgres). */}}
{{- define "dev-redis.chart" -}}
{{- printf "%s-%s" .Chart.Name (.Chart.Version | toString | splitList "+" | first) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dev-redis.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dev-redis.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dev-redis.labels" -}}
helm.sh/chart: {{ include "dev-redis.chart" . }}
{{ include "dev-redis.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
