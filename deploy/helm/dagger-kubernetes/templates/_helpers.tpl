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

{{/* Resolve the cache public vhost (the host engines push/pull through the
Supervisor proxy). Explicit value wins; otherwise derive `cache.<host>` from
supervisor.config.server.publicUrl by stripping the scheme and any trailing
path. Assumes publicUrl has no explicit port (the cache vhost must match the
ingress Host header / TLS SAN, which never carry a port). */}}
{{- define "dagger-kubernetes.cachePublicHost" -}}
{{- if .Values.supervisor.config.cache.publicHost -}}
{{- .Values.supervisor.config.cache.publicHost -}}
{{- else -}}
{{- $u := trimPrefix "https://" .Values.supervisor.config.server.publicUrl -}}
{{- $u = trimPrefix "http://" $u -}}
{{- $u = regexReplaceAll "/.*$" $u "" -}}
{{- printf "cache.%s" $u -}}
{{- end -}}
{{- end -}}

{{/* Resolve the public OCI cache ref used in _EXPERIMENTAL_DAGGER_CACHE_CONFIG:
<cachePublicHost>/dagger-cache. The repo path is fixed to `dagger-cache`. */}}
{{- define "dagger-kubernetes.cacheRegistry" -}}
{{- printf "%s/dagger-cache" (include "dagger-kubernetes.cachePublicHost" .) -}}
{{- end -}}

{{/* Resolve the internal cache backend address (host[:port], no scheme): the
explicit legacy value, else the in-cluster registry Service when enabled. */}}
{{- define "dagger-kubernetes.cacheInternalAddr" -}}
{{- if .Values.supervisor.config.cache.internalAddr -}}
{{- .Values.supervisor.config.cache.internalAddr -}}
{{- else if .Values.tools.registry.enabled -}}
{{- printf "%s-registry:5000" .Release.Name -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Tempo URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- if .Values.tools.tempo.enabled -}}
{{- printf "http://%s-tempo:3200" .Release.Name -}}
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
{{- if .Values.dataIngress.enabled -}}
{{- .Values.dataIngress.host -}}
{{- else if eq .Values.service.data.type "NodePort" -}}
{{- printf "%s:%s" .Values.supervisor.config.server.dataHostname (.Values.service.data.nodePort | default "30443") -}}
{{- else -}}
{{- printf "%s:443" .Values.supervisor.config.server.dataHostname -}}
{{- end -}}
{{- end -}}

{{/* Resolve the supervisor StatefulSet name used for raft DNS peer discovery
(<sts>-<i>.<headless>.<ns>.svc.<clusterDomain>). Defaults to the chart fullname;
an explicit override supports supervisors managed outside this chart. */}}
{{- define "dagger-kubernetes.supervisorStatefulSetName" -}}
{{- default (include "dagger-kubernetes.fullname" .) .Values.supervisor.config.raft.statefulsetName -}}
{{- end -}}

{{/* Resolve the raft headless Service name (clusterIP: None) whose DNS A records
back the stable pod names used for discovery. Defaults to <fullname>-headless. */}}
{{- define "dagger-kubernetes.supervisorHeadlessService" -}}
{{- default (printf "%s-headless" (include "dagger-kubernetes.fullname" .)) .Values.supervisor.config.raft.headlessService -}}
{{- end -}}

{{/* Resolve the internal raft CA Secret name (shared CA cert+key across pods).
Defaults to <fullname>-raft-ca; an explicit override supports externally
managed CAs. */}}
{{- define "dagger-kubernetes.supervisorRaftCASecret" -}}
{{- default (printf "%s-raft-ca" (include "dagger-kubernetes.fullname" .)) .Values.supervisor.config.raft.tls.caSecret -}}
{{- end -}}

{{/* Resolve the control/data-plane server TLS certificate path. The embedded
provider issues its own server cert from the minting CA (under tls.ca_path), so
the path is unused. The cert-manager provider reads the cert-manager-issued
keypair mounted at /etc/dagger-kubernetes/data-tls (tls.crt). The external
provider uses the operator-supplied path. */}}
{{- define "dagger-kubernetes.controlTLSCertPath" -}}
{{- if eq (.Values.supervisor.config.tls.provider | default "embedded") "cert-manager" -}}
/etc/dagger-kubernetes/data-tls/tls.crt
{{- else -}}
{{- .Values.supervisor.config.tls.certPath | default "" -}}
{{- end -}}
{{- end -}}

{{- define "dagger-kubernetes.controlTLSKeyPath" -}}
{{- if eq (.Values.supervisor.config.tls.provider | default "embedded") "cert-manager" -}}
/etc/dagger-kubernetes/data-tls/tls.key
{{- else -}}
{{- .Values.supervisor.config.tls.keyPath | default "" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the VictoriaMetrics URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.victoriaUrl" -}}
{{- if .Values.tools.victoria.enabled -}}
{{- printf "http://%s-victoria-server:8428" .Release.Name -}}
{{- else -}}
{{- default "http://victoria:8428" .Values.supervisor.config.telemetry.victoriaUrl -}}
{{- end -}}
{{- end -}}
