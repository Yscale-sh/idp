{{- define "idp-shipper.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name (.Chart.Version | toString | splitList "+" | first) | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: idp-shipper
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
platform/managed-by: platformctl
{{- end }}
