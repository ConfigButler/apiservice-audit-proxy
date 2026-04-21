{{/*
Expand the chart name.
*/}}
{{- define "audit-pass-through-apiserver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "audit-pass-through-apiserver.fullname" -}}
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
Create chart label text.
*/}}
{{- define "audit-pass-through-apiserver.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "audit-pass-through-apiserver.labels" -}}
helm.sh/chart: {{ include "audit-pass-through-apiserver.chart" . }}
{{ include "audit-pass-through-apiserver.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "audit-pass-through-apiserver.selectorLabels" -}}
app.kubernetes.io/name: {{ include "audit-pass-through-apiserver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "audit-pass-through-apiserver.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "audit-pass-through-apiserver.fullname" .) .Values.serviceAccount.name }}
{{- else if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- fail "serviceAccount.name must be set when serviceAccount.create=false" }}
{{- end }}
{{- end }}

{{/*
Standard volume paths.
*/}}
{{- define "audit-pass-through-apiserver.paths.webhook" -}}/var/run/audit-pass-through/webhook{{- end }}
{{- define "audit-pass-through-apiserver.paths.tls" -}}/var/run/audit-pass-through/tls{{- end }}
{{- define "audit-pass-through-apiserver.paths.backendCA" -}}/var/run/audit-pass-through/backend-ca{{- end }}
{{- define "audit-pass-through-apiserver.paths.backendClient" -}}/var/run/audit-pass-through/backend-client{{- end }}

