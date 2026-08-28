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

{{/* Resolve the OTLP collector URL: use the dependency Service when enabled.
Always the <service>.<namespace>.svc form (see CONTRIBUTING.md): a single
`.svc` NO_PROXY entry exempts every in-cluster component from the proxy. */}}
{{- define "dagger-kubernetes.collectorUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s-opentelemetry-collector.%s.svc:4318" .Release.Name $ns -}}
{{- if index .Values "opentelemetry-collector" "enabled" -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.collectorUrl -}}
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
the in-cluster registry Service when the registry subchart is enabled, in the
<service>.<namespace>.svc form (see CONTRIBUTING.md). */}}
{{- define "dagger-kubernetes.cacheInternalAddr" -}}
{{- if .Values.registry.enabled -}}
{{- printf "%s-registry.%s.svc:5000" .Release.Name (include "dagger-kubernetes.namespace" .) -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Tempo URL: use the dependency Service when enabled, in the
<service>.<namespace>.svc form (see CONTRIBUTING.md). */}}
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s-tempo.%s.svc:3200" .Release.Name $ns -}}
{{- if .Values.tempo.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.tempoUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the Loki URL: use the dependency Service when enabled, in the
<service>.<namespace>.svc form (see CONTRIBUTING.md). */}}
{{- define "dagger-kubernetes.lokiUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s-loki.%s.svc:3100" .Release.Name $ns -}}
{{- if .Values.loki.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.lokiUrl -}}
{{- end -}}
{{- end -}}

{{/* Resolve the VictoriaMetrics URL: use the dependency Service when enabled,
in the <service>.<namespace>.svc form (see CONTRIBUTING.md). */}}
{{- define "dagger-kubernetes.victoriaUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s-victoria-server.%s.svc:8428" .Release.Name $ns -}}
{{- if .Values.victoria.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.victoriaUrl -}}
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

{{/* Resolve the data-plane server TLS provider. The chart auto-switches:
- dataIngress.tls.secretName -> "external" (operator/cert-manager-managed
  tls.crt/tls.key Secret, mounted at /etc/dagger-kubernetes/data-tls)
- dataCert.enabled -> "cert-manager" (chart-rendered Certificate, mounted at
  /etc/dagger-kubernetes/data-tls)
- otherwise the configured supervisor.dataplane.tls.provider (default
  "embedded"). When both dataCert.enabled and dataIngress.tls.secretName are
  set, dataIngress.tls.secretName wins (that secret is mounted and served;
  cert-manager's dataCert secret is unused). */}}
{{- define "dagger-kubernetes.dataplaneTLSProvider" -}}
{{- if .Values.dataIngress.tls.secretName -}}
external
{{- else if .Values.dataCert.enabled -}}
cert-manager
{{- else -}}
{{- .Values.supervisor.dataplane.tls.provider | default "embedded" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the control/data-plane server TLS certificate path. The embedded
provider issues its own server cert from the minting CA (under
supervisor.dataplane.tls.ca_path), so the path is unused. When dataCert or
dataIngress.tls.secretName is set, the chart auto-wires the mounted
data-tls Secret (/etc/dagger-kubernetes/data-tls/tls.crt). The external
provider reads the <fullname>-tls Secret mounted at
/etc/dagger-kubernetes/tls when supervisor.dataplane.tls.crt/tls.key are set
(the chart auto-wires certPath/keyPath to it), or the operator-supplied path
when supervisor.dataplane.tls.certPath/tls.keyPath are set explicitly. */}}
{{- define "dagger-kubernetes.dataplaneTLSCertPath" -}}
{{- if or .Values.dataCert.enabled .Values.dataIngress.tls.secretName -}}
/etc/dagger-kubernetes/data-tls/tls.crt
{{- else if eq (include "dagger-kubernetes.dataplaneTLSProvider" .) "external" -}}
{{- if and .Values.supervisor.dataplane.tls.crt .Values.supervisor.dataplane.tls.key -}}
/etc/dagger-kubernetes/tls/tls.crt
{{- else -}}
{{- .Values.supervisor.dataplane.tls.certPath | default "" -}}
{{- end -}}
{{- else -}}
{{- .Values.supervisor.dataplane.tls.certPath | default "" -}}
{{- end -}}
{{- end -}}

{{- define "dagger-kubernetes.dataplaneTLSKeyPath" -}}
{{- if or .Values.dataCert.enabled .Values.dataIngress.tls.secretName -}}
/etc/dagger-kubernetes/data-tls/tls.key
{{- else if eq (include "dagger-kubernetes.dataplaneTLSProvider" .) "external" -}}
{{- if and .Values.supervisor.dataplane.tls.crt .Values.supervisor.dataplane.tls.key -}}
/etc/dagger-kubernetes/tls/tls.key
{{- else -}}
{{- .Values.supervisor.dataplane.tls.keyPath | default "" -}}
{{- end -}}
{{- else -}}
{{- .Values.supervisor.dataplane.tls.keyPath | default "" -}}
{{- end -}}
{{- end -}}
