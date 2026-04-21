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
Serving certificate mode.
*/}}
{{- define "audit-pass-through-apiserver.certificateMode" -}}
{{- $mode := default "existing-secret" .Values.certificates.mode -}}
{{- if or (eq $mode "existing-secret") (eq $mode "dev-self-signed") (eq $mode "cert-manager") -}}
{{- $mode -}}
{{- else -}}
{{- fail (printf "unsupported certificates.mode %q" $mode) -}}
{{- end -}}
{{- end }}

{{/*
Serving TLS Secret name.
*/}}
{{- define "audit-pass-through-apiserver.servingSecretName" -}}
{{- $mode := include "audit-pass-through-apiserver.certificateMode" . -}}
{{- if eq $mode "existing-secret" -}}
{{- required "certificates.existingSecretName is required when certificates.mode=existing-secret" .Values.certificates.existingSecretName -}}
{{- else if eq $mode "dev-self-signed" -}}
{{- default (printf "%s-dev-tls" (include "audit-pass-through-apiserver.fullname" .)) .Values.certificates.devSelfSigned.secretName -}}
{{- else -}}
{{- default (printf "%s-tls" (include "audit-pass-through-apiserver.fullname" .)) .Values.certificates.certManager.secretName -}}
{{- end -}}
{{- end }}

{{/*
Serving certificate name for cert-manager mode.
*/}}
{{- define "audit-pass-through-apiserver.servingCertificateName" -}}
{{- default (printf "%s-serving" (include "audit-pass-through-apiserver.fullname" .)) .Values.certificates.certManager.certificateName -}}
{{- end }}

{{/*
Serving issuer name for cert-manager mode.
*/}}
{{- define "audit-pass-through-apiserver.servingIssuerName" -}}
{{- if .Values.certificates.certManager.issuerRef.name -}}
{{- .Values.certificates.certManager.issuerRef.name -}}
{{- else if .Values.certificates.certManager.createSelfSignedIssuer -}}
{{- default (printf "%s-selfsigned" (include "audit-pass-through-apiserver.fullname" .)) .Values.certificates.certManager.selfSignedIssuerName -}}
{{- else -}}
{{- fail "certificates.certManager.issuerRef.name is required when certificates.mode=cert-manager and createSelfSignedIssuer=false" -}}
{{- end -}}
{{- end }}

{{/*
Default DNS names for proxy serving TLS.
*/}}
{{- define "audit-pass-through-apiserver.servingDNSNames" -}}
{{- $serviceName := include "audit-pass-through-apiserver.fullname" . -}}
- {{ $serviceName }}
- {{ printf "%s.%s" $serviceName .Release.Namespace }}
- {{ printf "%s.%s.svc" $serviceName .Release.Namespace }}
- {{ printf "%s.%s.svc.cluster.local" $serviceName .Release.Namespace }}
{{- end }}

{{/*
Should the APIService skip TLS verification.
*/}}
{{- define "audit-pass-through-apiserver.apiServiceSkipVerify" -}}
{{- if or .Values.apiService.insecureSkipTLSVerify (eq (include "audit-pass-through-apiserver.certificateMode" .) "dev-self-signed") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Standard volume paths.
*/}}
{{- define "audit-pass-through-apiserver.paths.webhook" -}}/var/run/audit-pass-through/webhook{{- end }}
{{- define "audit-pass-through-apiserver.paths.tls" -}}/var/run/audit-pass-through/tls{{- end }}
{{- define "audit-pass-through-apiserver.paths.backendCA" -}}/var/run/audit-pass-through/backend-ca{{- end }}
{{- define "audit-pass-through-apiserver.paths.backendClient" -}}/var/run/audit-pass-through/backend-client{{- end }}
{{- define "audit-pass-through-apiserver.paths.requestHeaderCA" -}}/var/run/audit-pass-through/requestheader-client-ca{{- end }}
