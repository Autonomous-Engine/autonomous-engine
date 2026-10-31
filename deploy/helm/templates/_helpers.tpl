{{/*
Expand the name of the chart.
*/}}
{{- define "autonomous-engine.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "autonomous-engine.fullname" -}}
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
{{- define "autonomous-engine.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "autonomous-engine.labels" -}}
helm.sh/chart: {{ include "autonomous-engine.chart" . }}
{{ include "autonomous-engine.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "autonomous-engine.selectorLabels" -}}
app.kubernetes.io/name: {{ include "autonomous-engine.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service-specific selector labels — pass a dict with "root" (.) and "service" (string)
*/}}
{{- define "autonomous-engine.serviceSelectorLabels" -}}
app.kubernetes.io/name: {{ .service }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{/*
Service-specific labels — pass a dict with "root" (.) and "service" (string)
*/}}
{{- define "autonomous-engine.serviceLabels" -}}
helm.sh/chart: {{ include "autonomous-engine.chart" .root }}
app.kubernetes.io/name: {{ .service }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- if .root.Chart.AppVersion }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end }}

{{/*
Image tag: image.tag when set, otherwise the release tag the publish workflow
(.github/workflows/publish-images.yml) pushes for this chart's appVersion,
"v" + appVersion (appVersion 0.1.0 -> v0.1.0).
*/}}
{{- define "autonomous-engine.imageTag" -}}
{{- if .Values.image.tag }}
{{- .Values.image.tag }}
{{- else }}
{{- printf "v%s" (trimPrefix "v" .Chart.AppVersion) }}
{{- end }}
{{- end }}

{{/*
Image of one service — pass a dict with "root" (.) and "service" (string).
Every service has its own image, published as
<image.registry>/<image.repositoryPrefix><service>:<tag>, for example
ghcr.io/autonomous-engine/ae-gateway:v0.1.0.
*/}}
{{- define "autonomous-engine.image" -}}
{{- $v := .root.Values.image }}
{{- printf "%s/%s%s:%s" $v.registry $v.repositoryPrefix .service (include "autonomous-engine.imageTag" .root) }}
{{- end }}

{{/*
REDIS_URL for the services: redis.url when set (an external Redis), the
in-chart Redis when redis.enabled, otherwise empty (Redis disabled).
*/}}
{{- define "autonomous-engine.redisURL" -}}
{{- if .Values.redis.url }}
{{- .Values.redis.url }}
{{- else if .Values.redis.enabled }}
{{- printf "redis://redis:%d/0" (.Values.redis.port | int) }}
{{- end }}
{{- end }}
