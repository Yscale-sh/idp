{{/*
Expand the name of the chart.
*/}}
{{- define "dev-postgres.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "dev-postgres.fullname" -}}
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

{{/*
Chart name + version label. The version is stripped of any build metadata (the
"+<sha>" Flux appends under reconcileStrategy: Revision) so helm.sh/chart stays
STABLE across commits — otherwise it churns the StatefulSet's IMMUTABLE
volumeClaimTemplates labels (Forbidden on upgrade) and rolls pods on every commit.
*/}}
{{- define "dev-postgres.chart" -}}
{{- printf "%s-%s" .Chart.Name (.Chart.Version | toString | splitList "+" | first) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "dev-postgres.labels" -}}
helm.sh/chart: {{ include "dev-postgres.chart" . }}
{{ include "dev-postgres.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: database
platform/managed-by: platformctl
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "dev-postgres.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dev-postgres.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the Secret that holds the credentials. When auth.existingSecret is set we
reference that; otherwise we manage <fullname>.
*/}}
{{- define "dev-postgres.secretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- include "dev-postgres.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
Resolve the effective password:
  1. auth.password if explicitly set,
  2. else the value already stored in the live Secret (preserved across upgrades),
  3. else a freshly generated randAlphaNum 24.
Only used when auth.existingSecret is empty (otherwise the external Secret owns it).
*/}}
{{- define "dev-postgres.password" -}}
{{- if .Values.auth.password -}}
{{- .Values.auth.password -}}
{{- else -}}
{{- $secretName := include "dev-postgres.fullname" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data "POSTGRES_PASSWORD") -}}
{{- index $existing.data "POSTGRES_PASSWORD" | b64dec -}}
{{- else -}}
{{- randAlphaNum 24 -}}
{{- end -}}
{{- end -}}
{{- end }}
