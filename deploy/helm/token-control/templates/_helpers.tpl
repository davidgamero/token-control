{{/*
Expand the name of the chart.
*/}}
{{- define "token-control.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a fully qualified app name. Truncated to 63 chars for DNS-label constraints.
*/}}
{{- define "token-control.fullname" -}}
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

{{/*
Chart name and version label value.
*/}}
{{- define "token-control.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The namespace the release is installed into.
*/}}
{{- define "token-control.namespace" -}}
{{- .Release.Namespace -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "token-control.labels" -}}
helm.sh/chart: {{ include "token-control.chart" . }}
{{ include "token-control.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: token-control
{{- end -}}

{{/*
Selector labels (stable across upgrades; do not add version here).
*/}}
{{- define "token-control.selectorLabels" -}}
app.kubernetes.io/name: {{ include "token-control.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The ServiceAccount name to use.
*/}}
{{- define "token-control.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "token-control.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The fully-qualified manager image reference.
*/}}
{{- define "token-control.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Name of the webhook serving-certificate Secret.
*/}}
{{- define "token-control.webhookCertSecret" -}}
{{- printf "%s-webhook-cert" (include "token-control.fullname" .) -}}
{{- end -}}

{{/*
Name of the webhook Service.
*/}}
{{- define "token-control.webhookServiceName" -}}
{{- printf "%s-webhook" (include "token-control.fullname" .) -}}
{{- end -}}

{{/*
gen-certs memoizes a self-signed CA + serving certificate for the webhook on the release
context (.Values._webhookCerts) so every template that needs the caBundle or the TLS Secret
sees the *same* material within a single render. On upgrades, an already-issued certificate
is reused (via lookup) so rotating it is opt-in and the running webhook is not disrupted.

Values stored are base64-encoded and ready to drop into Secret .data / webhook caBundle.
Only used when cert-manager integration is disabled.
*/}}
{{- define "token-control.gen-certs" -}}
{{- if not .Values._webhookCerts -}}
{{- $fullName := include "token-control.fullname" . -}}
{{- $ns := include "token-control.namespace" . -}}
{{- $secretName := include "token-control.webhookCertSecret" . -}}
{{- $svc := include "token-control.webhookServiceName" . -}}
{{- $cn := printf "%s.%s.svc" $svc $ns -}}
{{- $altNames := list $cn (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $existing := lookup "v1" "Secret" $ns $secretName -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") (index $existing.data "ca.crt") -}}
{{- $_ := set .Values "_webhookCerts" (dict "caCert" (index $existing.data "ca.crt") "tlsCert" (index $existing.data "tls.crt") "tlsKey" (index $existing.data "tls.key")) -}}
{{- else -}}
{{- $ca := genCA (printf "%s-ca" $fullName) 3650 -}}
{{- $cert := genSignedCert $cn nil $altNames 3650 $ca -}}
{{- $_ := set .Values "_webhookCerts" (dict "caCert" ($ca.Cert | b64enc) "tlsCert" ($cert.Cert | b64enc) "tlsKey" ($cert.Key | b64enc)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
caBundle to embed in the webhook configurations. Empty when cert-manager injects it via
the cert-manager.io/inject-ca-from annotation.
*/}}
{{- define "token-control.caBundle" -}}
{{- if .Values.webhook.certManager.enabled -}}
{{- else -}}
{{- include "token-control.gen-certs" . -}}
{{- .Values._webhookCerts.caCert -}}
{{- end -}}
{{- end -}}
