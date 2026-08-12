{{/* Common helpers for dagger-kubernetes chart */}}
{{- define "dagger-kubernetes.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "dagger-kubernetes.fullname" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "dagger-kubernetes.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "dagger-kubernetes.labels" -}}
helm.sh/chart: {{ include "dagger-kubernetes.chart" . }}
{{ include "dagger-kubernetes.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "dagger-kubernetes.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dagger-kubernetes.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "dagger-kubernetes.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}

{{/* Resolve the OTLP collector URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.collectorUrl" -}}
{{- if .Values.tools.otelCollector.enabled -}}
{{- printf "http://%s-opentelemetry-collector:4318" .Release.Name -}}
{{- else -}}
{{- default (printf "http://%s-opentelemetry-collector:4318" .Release.Name) .Values.supervisor.config.telemetry.collectorUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the OCI cache registry: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.cacheRegistry" -}}
{{- if .Values.tools.registry.enabled -}}
{{- printf "%s-docker-registry:5000/dagger-cache" .Release.Name -}}
{{- else -}}
{{- default (printf "%s-docker-registry:5000/dagger-cache" .Release.Name) .Values.supervisor.config.cache.registry -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Tempo URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- if .Values.tools.tempo.enabled -}}
{{- printf "http://%s-tempo:3100" .Release.Name -}}
{{- else -}}
{{- default "http://tempo:3200" .Values.supervisor.config.telemetry.tempoUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Loki URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.lokiUrl" -}}
{{- if .Values.tools.loki.enabled -}}
{{- printf "http://%s-loki:3100" .Release.Name -}}
{{- else -}}
{{- default "http://loki:3100" .Values.supervisor.config.telemetry.lokiUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the data-plane hostname from the configured access method. */}}
{{- define "dagger-kubernetes.dataHostname" -}}
{{- if .Values.service.data.ingress.enabled -}}
{{- printf "%s:%d" .Values.supervisor.config.server.dataHostname (int .Values.service.data.ingress.port) -}}
{{- else if eq .Values.service.data.type "NodePort" -}}
{{- printf "%s:%s" .Values.supervisor.config.server.dataHostname (.Values.service.data.nodePort | default "30443") -}}
{{- else -}}
{{- printf "%s:443" .Values.supervisor.config.server.dataHostname -}}
{{- end -}}
{{- end -}}

{{/* Resolve the VictoriaMetrics URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.victoriaUrl" -}}
{{- if .Values.tools.victoria.enabled -}}
{{- printf "http://%s-victoria-metrics-single:8428" .Release.Name -}}
{{- else -}}
{{- default "http://victoria:8428" .Values.supervisor.config.telemetry.victoriaUrl -}}
{{- end -}}
{{- end -}}
