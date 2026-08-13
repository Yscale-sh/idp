{{/* Base name for this release's objects. */}}
{{- define "bucketProvisioner.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Name of the Secret external-secrets materializes for the Job. */}}
{{- define "bucketProvisioner.secretName" -}}
{{- printf "%s-creds" (include "bucketProvisioner.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Identity of one provisioning attempt. A Job's pod template is IMMUTABLE, so the
Job name has to change whenever anything the Job acts on changes — otherwise the
hook re-creates a Job whose spec no longer matches and the release wedges.

The CREDENTIAL REFS are part of that identity: rotating the profile onto a
different key pair must re-run provisioning (the new pair may not own the
bucket yet), and it is the only signal we have — the values themselves never
reach this chart. The refs are non-secret names, not the credentials.
*/}}
{{- define "bucketProvisioner.identity" -}}
{{- $c := .Values.credentials -}}
{{- printf "%s|%s|%s|%v|%s|%s|%s|%s|%s" .Values.bucket .Values.endpoint .Values.region .Values.pathStyle $c.storeRef.name (default "" $c.storeRef.kind) $c.accessKeyID.key (default "" $c.accessKeyID.property) $c.secretAccessKey.key -}}
{{- end -}}

{{- define "bucketProvisioner.jobName" -}}
{{- $hash := include "bucketProvisioner.identity" . | sha256sum | trunc 8 -}}
{{- printf "%s-%s" (include "bucketProvisioner.fullname" . | trunc 54 | trimSuffix "-") $hash -}}
{{- end -}}

{{- define "bucketProvisioner.labels" -}}
app.kubernetes.io/name: bucket-provisioner
app.kubernetes.io/instance: {{ include "bucketProvisioner.fullname" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.labels }}
{{ toYaml . }}
{{- end }}
{{- end -}}
