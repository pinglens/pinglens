{{/*
Expand the name of the chart.
*/}}
{{- define "pinglens_exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "pinglens_exporter.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "pinglens_exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "pinglens_exporter.labels" -}}
helm.sh/chart: {{ include "pinglens_exporter.chart" . }}
{{ include "pinglens_exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "pinglens_exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pinglens_exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "pinglens_exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pinglens_exporter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
One config.yml target in the ping_exporter v1.2+ style the exporter reads:
the address under "host" and every other key a label. Labels must be
strings, so values are quoted. Takes a dict with "host" and "labels".
*/}}
{{- define "pinglens_exporter.target" -}}
{{- if hasKey .labels "host" }}
{{- fail (printf "pinglens-exporter: target %s: \"host\" is reserved and cannot be a label" .host) }}
{{- end -}}
- host: {{ .host | quote }}
{{- range $name, $value := .labels }}
  {{ $name }}: {{ $value | toString | quote }}
{{- end }}
{{- end }}
