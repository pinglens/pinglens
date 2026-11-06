{{/* Expand the name of the chart. */}}
{{- define "pinglens.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a default fully qualified app name. */}}
{{- define "pinglens.fullname" -}}
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

{{/* Create chart name and version as used by the chart label. */}}
{{- define "pinglens.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels */}}
{{- define "pinglens.labels" -}}
helm.sh/chart: {{ include "pinglens.chart" . }}
{{ include "pinglens.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Selector labels */}}
{{- define "pinglens.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pinglens.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Service account name */}}
{{- define "pinglens.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pinglens.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ConfigMap name */}}
{{- define "pinglens.configName" -}}
{{- printf "%s-config" (include "pinglens.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Coordinator resource name */}}
{{- define "pinglens.coordinatorFullname" -}}
{{ include "pinglens.fullname" . }}
{{- end }}

{{/* Agent resource name */}}
{{- define "pinglens.agentFullname" -}}
{{- if .Values.coordinator.enabled -}}
{{- printf "%s-agent" (include "pinglens.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{ include "pinglens.fullname" . }}
{{- end }}
{{- end }}

{{/* Coordinator ConfigMap name */}}
{{- define "pinglens.coordinatorConfigName" -}}
{{- printf "%s-config" (include "pinglens.coordinatorFullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Agent ConfigMap name */}}
{{- define "pinglens.agentConfigName" -}}
{{- printf "%s-config" (include "pinglens.agentFullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

