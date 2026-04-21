{{/*
Expand the chart name.
*/}}
{{- define "apiservice-audit-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "apiservice-audit-proxy.fullname" -}}
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
{{- define "apiservice-audit-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "apiservice-audit-proxy.labels" -}}
helm.sh/chart: {{ include "apiservice-audit-proxy.chart" . }}
{{ include "apiservice-audit-proxy.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "apiservice-audit-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "apiservice-audit-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "apiservice-audit-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "apiservice-audit-proxy.fullname" .) .Values.serviceAccount.name }}
{{- else if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- fail "serviceAccount.name must be set when serviceAccount.create=false" }}
{{- end }}
{{- end }}

{{/*
Serving certificate mode.
*/}}
{{- define "apiservice-audit-proxy.certificateMode" -}}
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
{{- define "apiservice-audit-proxy.servingSecretName" -}}
{{- $mode := include "apiservice-audit-proxy.certificateMode" . -}}
{{- if eq $mode "existing-secret" -}}
{{- required "certificates.existingSecretName is required when certificates.mode=existing-secret" .Values.certificates.existingSecretName -}}
{{- else if eq $mode "dev-self-signed" -}}
{{- default (printf "%s-dev-tls" (include "apiservice-audit-proxy.fullname" .)) .Values.certificates.devSelfSigned.secretName -}}
{{- else -}}
{{- default (printf "%s-tls" (include "apiservice-audit-proxy.fullname" .)) .Values.certificates.certManager.secretName -}}
{{- end -}}
{{- end }}

{{/*
Serving certificate name for cert-manager mode.
*/}}
{{- define "apiservice-audit-proxy.servingCertificateName" -}}
{{- default (printf "%s-serving" (include "apiservice-audit-proxy.fullname" .)) .Values.certificates.certManager.certificateName -}}
{{- end }}

{{/*
Serving issuer name for cert-manager mode.
*/}}
{{- define "apiservice-audit-proxy.servingIssuerName" -}}
{{- if .Values.certificates.certManager.issuerRef.name -}}
{{- .Values.certificates.certManager.issuerRef.name -}}
{{- else if .Values.certificates.certManager.createSelfSignedIssuer -}}
{{- default (printf "%s-selfsigned" (include "apiservice-audit-proxy.fullname" .)) .Values.certificates.certManager.selfSignedIssuerName -}}
{{- else -}}
{{- fail "certificates.certManager.issuerRef.name is required when certificates.mode=cert-manager and createSelfSignedIssuer=false" -}}
{{- end -}}
{{- end }}

{{/*
Default DNS names for proxy serving TLS.
*/}}
{{- define "apiservice-audit-proxy.servingDNSNames" -}}
{{- $serviceName := include "apiservice-audit-proxy.fullname" . -}}
- {{ $serviceName }}
- {{ printf "%s.%s" $serviceName .Release.Namespace }}
- {{ printf "%s.%s.svc" $serviceName .Release.Namespace }}
- {{ printf "%s.%s.svc.cluster.local" $serviceName .Release.Namespace }}
{{- end }}

{{/*
Should the APIService skip TLS verification.
*/}}
{{- define "apiservice-audit-proxy.apiServiceSkipVerify" -}}
{{- if or .Values.apiService.insecureSkipTLSVerify (eq (include "apiservice-audit-proxy.certificateMode" .) "dev-self-signed") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Standard volume paths.
*/}}
{{- define "apiservice-audit-proxy.paths.webhook" -}}/var/run/audit-pass-through/webhook{{- end }}
{{- define "apiservice-audit-proxy.paths.tls" -}}/var/run/audit-pass-through/tls{{- end }}
{{- define "apiservice-audit-proxy.paths.backendCA" -}}/var/run/audit-pass-through/backend-ca{{- end }}
{{- define "apiservice-audit-proxy.paths.backendClient" -}}/var/run/audit-pass-through/backend-client{{- end }}
{{- define "apiservice-audit-proxy.paths.requestHeaderCA" -}}/var/run/audit-pass-through/requestheader-client-ca{{- end }}
