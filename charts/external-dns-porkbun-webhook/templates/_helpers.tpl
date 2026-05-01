{{/* Standard helm chart helpers. */}}

{{- define "edns-porkbun.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "edns-porkbun.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "edns-porkbun.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "edns-porkbun.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "edns-porkbun.selectorLabels" -}}
app.kubernetes.io/name: {{ include "edns-porkbun.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "edns-porkbun.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{ default (include "edns-porkbun.fullname" .) .Values.serviceAccount.name }}
{{- else -}}
{{ default "default" .Values.serviceAccount.name }}
{{- end -}}
{{- end -}}

{{- define "edns-porkbun.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{ printf "%s:%s" .Values.image.repository $tag }}
{{- end -}}

{{/* Build the env vars for credentials and config. */}}
{{- define "edns-porkbun.env" -}}
- name: PORKBUN_DOMAIN
  value: {{ required ".Values.porkbun.domain is required" .Values.porkbun.domain | quote }}
{{- if .Values.porkbun.domainFilter }}
- name: DOMAIN_FILTER
  value: {{ join "," .Values.porkbun.domainFilter | quote }}
{{- end }}
- name: DRY_RUN
  value: {{ .Values.dryRun | quote }}
- name: CACHE_TTL
  value: {{ .Values.cacheTTL | quote }}
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: LOG_FORMAT
  value: {{ .Values.logFormat | quote }}
- name: WEBHOOK_LISTEN
  value: {{ .Values.webhookListen | quote }}
- name: OPS_LISTEN
  value: {{ .Values.opsListen | quote }}
{{- if .Values.porkbun.existingSecret.name }}
- name: PORKBUN_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.porkbun.existingSecret.name | quote }}
      key:  {{ .Values.porkbun.existingSecret.apiKeyKey | quote }}
- name: PORKBUN_SECRET_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.porkbun.existingSecret.name | quote }}
      key:  {{ .Values.porkbun.existingSecret.secretApiKeyKey | quote }}
{{- else }}
- name: PORKBUN_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "edns-porkbun.fullname" . }}-creds
      key:  PORKBUN_API_KEY
- name: PORKBUN_SECRET_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "edns-porkbun.fullname" . }}-creds
      key:  PORKBUN_SECRET_API_KEY
{{- end }}
{{- with .Values.extraEnv }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end -}}
