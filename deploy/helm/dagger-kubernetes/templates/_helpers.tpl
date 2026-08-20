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
{{- if index .Values "opentelemetry-collector" "enabled" -}}
{{- printf "http://%s-opentelemetry-collector:4318" .Release.Name -}}
{{- else -}}
{{- default (printf "http://%s-opentelemetry-collector:4318" .Release.Name) .Values.supervisor.config.telemetry.collectorUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the OAuth2 redirect URL: explicit value wins, else derived from
the computed public URL and the provider callback route. */}}
{{- define "dagger-kubernetes.oauthRedirectUrl" -}}
{{- if .Values.auth.oauth.redirectUrl -}}
{{- .Values.auth.oauth.redirectUrl -}}
{{- else -}}
{{- printf "%s/api/v1/auth/oauth/%s/callback" (include "dagger-kubernetes.publicUrl" .) .Values.auth.oauth.provider -}}
{{- end -}}
{{- end -}}

{{/* Resolve the cache public vhost (the host engines push/pull through the
Supervisor proxy). Explicit value wins; otherwise derive `cache.<host>` from
the computed public URL by stripping the scheme and any port/path. */}}
{{- define "dagger-kubernetes.cachePublicHost" -}}
{{- if .Values.supervisor.config.cache.publicHost -}}
{{- .Values.supervisor.config.cache.publicHost -}}
{{- else -}}
{{- $u := include "dagger-kubernetes.publicUrl" . -}}
{{- $u = trimPrefix "https://" $u -}}
{{- $u = trimPrefix "http://" $u -}}
{{- $u = regexReplaceAll "[:/].*$" $u "" -}}
{{- printf "cache.%s" $u -}}
{{- end -}}
{{- end -}}

{{/* Resolve the public OCI cache ref used in _EXPERIMENTAL_DAGGER_CACHE_CONFIG:
<cachePublicHost>/dagger-cache. The repo path is fixed to `dagger-cache`. */}}
{{- define "dagger-kubernetes.cacheRegistry" -}}
{{- printf "%s/dagger-cache" (include "dagger-kubernetes.cachePublicHost" .) -}}
{{- end -}}

{{/* Resolve the internal cache backend address (host[:port], no scheme):
the in-cluster registry Service when the registry subchart is enabled. */}}
{{- define "dagger-kubernetes.cacheInternalAddr" -}}
{{- if .Values.registry.enabled -}}
{{- printf "%s-registry:5000" .Release.Name -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Tempo URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- if .Values.tempo.enabled -}}
{{- printf "http://%s-tempo:3200" .Release.Name -}}
{{- else -}}
{{- default "http://tempo:3200" .Values.supervisor.config.telemetry.tempoUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Loki URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.lokiUrl" -}}
{{- if .Values.loki.enabled -}}
{{- printf "http://%s-loki:3100" .Release.Name -}}
{{- else -}}
{{- default "http://loki:3100" .Values.supervisor.config.telemetry.lokiUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the VictoriaMetrics URL: use the dependency Service when enabled. */}}
{{- define "dagger-kubernetes.victoriaUrl" -}}
{{- if .Values.victoria.enabled -}}
{{- printf "http://%s-victoria-server:8428" .Release.Name -}}
{{- else -}}
{{- default "http://victoria:8428" .Values.supervisor.config.telemetry.victoriaUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the public control-plane URL (UI + API) from the exposition:
- ingress: https when ingress.tls is set, http otherwise, host = first ingress host
- LoadBalancer/NodePort: https://<service.control.host>[:port]
- ClusterIP: internal https://<release>-control.<namespace>.svc:<port> */}}
{{- define "dagger-kubernetes.publicUrl" -}}
{{- if .Values.ingress.enabled -}}
{{- $host := "" -}}
{{- range .Values.ingress.hosts -}}
{{- if not $host -}}{{- $host = .host -}}{{- end -}}
{{- end -}}
{{- $host = required "ingress.hosts is required when ingress.enabled" $host -}}
{{- if .Values.ingress.tls -}}
{{- printf "https://%s" $host -}}
{{- else -}}
{{- printf "http://%s" $host -}}
{{- end -}}
{{- else if or (eq .Values.service.control.type "LoadBalancer") (eq .Values.service.control.type "NodePort") -}}
{{- $host := required "service.control.host is required when the control plane is exposed via LoadBalancer/NodePort without an ingress" .Values.service.control.host -}}
{{- if eq (int .Values.service.control.port) 443 -}}
{{- printf "https://%s" $host -}}
{{- else -}}
{{- printf "https://%s:%v" $host .Values.service.control.port -}}
{{- end -}}
{{- else -}}
{{- printf "https://%s-control.%s.svc:%v" (include "dagger-kubernetes.fullname" .) (include "dagger-kubernetes.namespace" .) .Values.service.control.port -}}
{{- end -}}
{{- end -}}

{{/* Resolve the data-plane hostname (host[:port], no scheme — the supervisor
appends :443 itself when no port is given) from the exposition:
- dataIngress: the passthrough host (TLS, port 443)
- LoadBalancer: <service.data.host>[:port]
- NodePort: <service.data.host>:<service.data.nodePort> (nodePort required)
- ClusterIP: internal <release>-data.<namespace>.svc:<port> */}}
{{- define "dagger-kubernetes.dataHostname" -}}
{{- if .Values.dataIngress.enabled -}}
{{- .Values.dataIngress.host -}}
{{- else if eq .Values.service.data.type "LoadBalancer" -}}
{{- $host := required "service.data.host is required when the data plane is exposed via LoadBalancer without dataIngress" .Values.service.data.host -}}
{{- if eq (int .Values.service.data.port) 443 -}}
{{- $host -}}
{{- else -}}
{{- printf "%s:%v" $host .Values.service.data.port -}}
{{- end -}}
{{- else if eq .Values.service.data.type "NodePort" -}}
{{- $host := required "service.data.host is required when the data plane is exposed via NodePort without dataIngress" .Values.service.data.host -}}
{{- $nodePort := required "service.data.nodePort is required when service.data.type=NodePort (the auto-assigned port is unknown to the chart)" .Values.service.data.nodePort -}}
{{- printf "%s:%v" $host $nodePort -}}
{{- else -}}
{{- printf "%s-data.%s.svc:%v" (include "dagger-kubernetes.fullname" .) (include "dagger-kubernetes.namespace" .) .Values.service.data.port -}}
{{- end -}}
{{- end -}}

{{/* Resolve the supervisor StatefulSet name used for raft DNS peer discovery
(<sts>-<i>.<headless>.<ns>.svc.<clusterDomain>). */}}
{{- define "dagger-kubernetes.supervisorStatefulSetName" -}}
{{- include "dagger-kubernetes.fullname" . -}}
{{- end -}}

{{/* Resolve the raft headless Service name (clusterIP: None) whose DNS A records
back the stable pod names used for discovery: <fullname>-headless. */}}
{{- define "dagger-kubernetes.supervisorHeadlessService" -}}
{{- printf "%s-headless" (include "dagger-kubernetes.fullname" .) -}}
{{- end -}}

{{/* Resolve the internal raft CA Secret name (shared CA cert+key across pods):
<fullname>-raft-ca. */}}
{{- define "dagger-kubernetes.supervisorRaftCASecret" -}}
{{- printf "%s-raft-ca" (include "dagger-kubernetes.fullname" .) -}}
{{- end -}}

{{/* Resolve the control/data-plane server TLS certificate path. The embedded
provider issues its own server cert from the minting CA (under tls.ca_path), so
the path is unused. The cert-manager provider reads the cert-manager-issued
keypair mounted at /etc/dagger-kubernetes/data-tls (tls.crt). The external
provider uses the operator-supplied path. */}}
{{- define "dagger-kubernetes.controlTLSCertPath" -}}
{{- if eq (.Values.tls.provider | default "embedded") "cert-manager" -}}
/etc/dagger-kubernetes/data-tls/tls.crt
{{- else -}}
{{- .Values.tls.certPath | default "" -}}
{{- end -}}
{{- end -}}

{{- define "dagger-kubernetes.controlTLSKeyPath" -}}
{{- if eq (.Values.tls.provider | default "embedded") "cert-manager" -}}
/etc/dagger-kubernetes/data-tls/tls.key
{{- else -}}
{{- .Values.tls.keyPath | default "" -}}
{{- end -}}
{{- end -}}
