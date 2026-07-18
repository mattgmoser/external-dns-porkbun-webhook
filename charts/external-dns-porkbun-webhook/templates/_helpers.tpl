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

{{/* Fail early rather than installing a Pod that can never start safely. */}}
{{- define "edns-porkbun.validateValues" -}}
{{- if not .Values.legacyStandalone.acceptRisk -}}
{{- fail "DEPRECATED: the standalone chart exposes an unauthenticated DNS mutation API. Use docs/external-dns-values.yaml, or explicitly set legacyStandalone.acceptRisk=true to accept the legacy topology" -}}
{{- end -}}
{{- if empty .Values.porkbun.domain -}}
{{- fail ".Values.porkbun.domain is required" -}}
{{- end -}}
{{- if and (empty .Values.porkbun.existingSecret.name) (or (empty .Values.porkbun.apiKey) (empty .Values.porkbun.secretApiKey)) -}}
{{- fail "set .Values.porkbun.existingSecret.name, or set both inline credentials for testing" -}}
{{- end -}}
{{- if or (lt (int .Values.replicaCount) 0) (gt (int .Values.replicaCount) 1) -}}
{{- fail ".Values.replicaCount must be 0 or 1 because the Porkbun rate limiter is process-local" -}}
{{- end -}}
{{- if or (lt (int .Values.containerPorts.webhook) 1) (gt (int .Values.containerPorts.webhook) 65535) -}}
{{- fail ".Values.containerPorts.webhook must be between 1 and 65535" -}}
{{- end -}}
{{- if or (lt (int .Values.containerPorts.ops) 1) (gt (int .Values.containerPorts.ops) 65535) -}}
{{- fail ".Values.containerPorts.ops must be between 1 and 65535" -}}
{{- end -}}
{{- if eq (int .Values.containerPorts.webhook) (int .Values.containerPorts.ops) -}}
{{- fail ".Values.containerPorts.webhook and .Values.containerPorts.ops must be different" -}}
{{- end -}}
{{- if ne .Values.service.type "ClusterIP" -}}
{{- fail ".Values.service.type must remain ClusterIP; never expose the unauthenticated webhook through a NodePort or LoadBalancer" -}}
{{- end -}}
{{- if or (lt (int .Values.service.webhookPort) 1) (gt (int .Values.service.webhookPort) 65535) -}}
{{- fail ".Values.service.webhookPort must be between 1 and 65535" -}}
{{- end -}}
{{- if or (lt (int .Values.service.metricsPort) 1) (gt (int .Values.service.metricsPort) 65535) -}}
{{- fail ".Values.service.metricsPort must be between 1 and 65535" -}}
{{- end -}}
{{- if eq (int .Values.service.webhookPort) (int .Values.service.metricsPort) -}}
{{- fail ".Values.service.webhookPort and .Values.service.metricsPort must be different" -}}
{{- end -}}
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
  value: {{ printf ":%v" .Values.containerPorts.webhook | quote }}
- name: OPS_LISTEN
  value: {{ printf ":%v" .Values.containerPorts.ops | quote }}
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
