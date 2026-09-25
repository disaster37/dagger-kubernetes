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

{{/* Compute a dependency subchart's Service name using that subchart's own
fullname rule (the shared "contains" pattern of grafana/tempo, grafana/loki,
minio, and opentelemetry-collector). Args (dict):
  name    - the subchart's effective Chart.Name (Helm sets .Chart.Name to the
            Chart.yaml alias when one is declared)
  values  - the subchart's values dict (e.g. .Values.tempo, .Values.minio)
  release - the release name
*/}}
{{- define "dagger-kubernetes.subchartServiceName" -}}
{{- $values := .values | default dict -}}
{{- if $values.fullnameOverride -}}
{{- $values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .name $values.nameOverride -}}
{{- if contains $name .release -}}
{{- .release | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .release $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "dagger-kubernetes.tempoServiceName" -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" "tempo" "values" .Values.tempo "release" .Release.Name) -}}
{{- end -}}

{{- define "dagger-kubernetes.lokiServiceName" -}}
{{- $name := "loki" -}}
{{- $enterprise := ((.Values.loki | default dict).enterprise | default dict) -}}
{{- if $enterprise.enabled -}}{{- $name = "enterprise-logs" -}}{{- end -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" $name "values" .Values.loki "release" .Release.Name) -}}
{{- end -}}

{{- define "dagger-kubernetes.minioServiceName" -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" "minio" "values" .Values.minio "release" .Release.Name) -}}
{{- end -}}

{{- define "dagger-kubernetes.otelCollectorServiceName" -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" "opentelemetry-collector" "values" (index .Values "opentelemetry-collector") "release" .Release.Name) -}}
{{- end -}}

{{/* VictoriaMetrics server Service name (victoria-metrics-single 0.44.0,
aliased "victoria" in Chart.yaml — Helm sets .Chart.Name to the alias inside
the subchart, so vm.fullname builds from "victoria"). Replicates
vm.plain.fullname: server.fullnameOverride wins, then the legacy
"<release>-victoria-server" name (default), or "vmvictoria-<release>" when
useLegacyNaming is false (vm.operator.kind derives the prefix from the aliased
chart name); server.useLegacyNaming overrides the chart-level key. */}}
{{- define "dagger-kubernetes.victoriaServerServiceName" -}}
{{- $sub := .Values.victoria | default dict -}}
{{- $server := $sub.server | default dict -}}
{{- if $server.fullnameOverride -}}
{{- $server.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $legacy := true -}}
{{- if hasKey $sub "useLegacyNaming" -}}
{{- $legacy = ne (toString $sub.useLegacyNaming) "false" -}}
{{- end -}}
{{- if hasKey $server "useLegacyNaming" -}}
{{- $legacy = ne (toString $server.useLegacyNaming) "false" -}}
{{- end -}}
{{- if not $legacy -}}
{{- printf "vmvictoria-%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $fo := default (($sub.global | default dict).fullnameOverride) $sub.fullnameOverride -}}
{{- if $fo -}}
{{- printf "%s-server" $fo | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "victoria" $sub.nameOverride -}}
{{- $base := "" -}}
{{- if contains $name .Release.Name -}}{{- $base = .Release.Name -}}{{- else -}}{{- $base = printf "%s-%s" .Release.Name $name -}}{{- end -}}
{{- printf "%s-server" $base | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Service names for the OTel collector's exporters. The collector config is
rendered by this chart (templates/otel-collector-configmap.yaml) with the
parent scope, so each name is derived from the subchart's own fullname rule
(tempoServiceName/lokiServiceName/victoriaServerServiceName) and honors its
fullnameOverride/nameOverride automatically. An explicit
global.daggerKubernetes.serviceNames.* entry wins over the derived name and is
meant only for backends the subcharts do not own (renamed Services, an
external observability stack). */}}
{{- define "dagger-kubernetes.otelTempoService" -}}
{{- $svc := (((.Values.global | default dict).daggerKubernetes | default dict).serviceNames | default dict) -}}
{{- default (include "dagger-kubernetes.tempoServiceName" .) $svc.tempo -}}
{{- end -}}

{{- define "dagger-kubernetes.otelLokiService" -}}
{{- $svc := (((.Values.global | default dict).daggerKubernetes | default dict).serviceNames | default dict) -}}
{{- default (include "dagger-kubernetes.lokiServiceName" .) $svc.loki -}}
{{- end -}}

{{- define "dagger-kubernetes.otelVictoriaService" -}}
{{- $svc := (((.Values.global | default dict).daggerKubernetes | default dict).serviceNames | default dict) -}}
{{- default (include "dagger-kubernetes.victoriaServerServiceName" .) $svc.victoria -}}
{{- end -}}

{{/* Resolve the OTLP collector URL: use the dependency Service when enabled.
Always the <service>.<namespace>.svc form (see CONTRIBUTING.md): a single
`.svc` NO_PROXY entry exempts every in-cluster component from the proxy. */}}
{{- define "dagger-kubernetes.collectorUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:4318" (include "dagger-kubernetes.otelCollectorServiceName" .) $ns -}}
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

{{/* Resolve the S3 endpoint for the supervisor's shared S3 client (CLI cache).
When the minio subchart is enabled, defaults to the in-cluster MinIO
service. */}}
{{- define "dagger-kubernetes.s3Endpoint" -}}
{{- if .Values.supervisor.config.cache.s3.endpoint -}}
{{- .Values.supervisor.config.cache.s3.endpoint -}}
{{- else if .Values.minio.enabled -}}
{{- printf "%s.%s.svc:%v" (include "dagger-kubernetes.minioServiceName" .) (include "dagger-kubernetes.namespace" .) (.Values.minio.service.port | default 9000) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the S3 bucket name. Falls back through the hierarchy:
cache s3 bucket -> MinIO default bucket. */}}
{{- define "dagger-kubernetes.s3Bucket" -}}
{{- $cacheBucket := .Values.supervisor.config.cache.s3.bucket -}}
{{- if $cacheBucket -}}
{{- $cacheBucket -}}
{{- else if .Values.minio.enabled -}}
{{- index (.Values.minio.buckets | default list) 0 | default dict | dig "name" "dagger-cache" -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the S3 access key. Defaults to the MinIO root user when minio is
enabled; falls back to the engine-s3-auth secret in all cases. */}}
{{- define "dagger-kubernetes.s3AccessKey" -}}
{{- if .Values.minio.enabled -}}
{{- .Values.minio.rootUser | default "minioadmin" -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the S3 secret key. Defaults to the MinIO root password when minio
is enabled; falls back to the engine-s3-auth secret in all cases. */}}
{{- define "dagger-kubernetes.s3SecretKey" -}}
{{- if .Values.minio.enabled -}}
{{- .Values.minio.rootPassword | default "minioadmin" -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/* Resolve the S3 endpoint for the image-cache mirrors. Explicit value wins;
otherwise the in-cluster MinIO service when the subchart is enabled. */}}
{{- define "dagger-kubernetes.imageCacheS3Endpoint" -}}
{{- if .Values.imageCache.storage.s3.endpoint -}}
{{- .Values.imageCache.storage.s3.endpoint -}}
{{- else if .Values.minio.enabled -}}
{{- printf "%s.%s.svc:%v" (include "dagger-kubernetes.minioServiceName" .) (include "dagger-kubernetes.namespace" .) (.Values.minio.service.port | default 9000) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/* Sanitize a registry host into a DNS-label slug: lowercase, every run of
non-[a-z0-9-] characters collapsed to a single "-", trimmed. */}}
{{- define "dagger-kubernetes.imageCacheSlug" -}}
{{- regexReplaceAll "[^a-z0-9]+" (. | lower) "-" | trimAll "-" -}}
{{- end -}}

{{/* Normalize the enabled image-cache upstreams into a YAML list of
{slug, host, remoteUrl, username, passwordSecretRef}. Presets carry a fixed
remoteUrl; custom registries supply their own. Fails on a slug collision so a
custom host cannot silently shadow a preset. */}}
{{- define "dagger-kubernetes.imageCacheRegistries" -}}
{{- $presets := dict
  "docker.io" "https://registry-1.docker.io"
  "ghcr.io" "https://ghcr.io"
  "public.ecr.aws" "https://public.ecr.aws"
  "quay.io" "https://quay.io"
  "gcr.io" "https://gcr.io"
  "registry.dagger.io" "https://registry.dagger.io" -}}
{{- $registries := list -}}
{{- range $host, $remote := $presets -}}
{{- $p := index $.Values.imageCache.presets $host | default dict -}}
{{- if $p.enabled -}}
{{- $registries = append $registries (dict "slug" (include "dagger-kubernetes.imageCacheSlug" $host) "host" $host "remoteUrl" $remote "username" ($p.username | default "") "passwordSecretRef" ($p.passwordSecretRef | default dict)) -}}
{{- end -}}
{{- end -}}
{{- range .Values.imageCache.registries -}}
{{- $host := required "imageCache.registries[]: host is required (the upstream registry hostname, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com)" .host -}}
{{- $remoteUrl := required (printf "imageCache.registries[]: remoteUrl is required for host %q (the upstream's https:// base URL, e.g. https://%s)" $host $host) .remoteUrl -}}
{{- $registries = append $registries (dict "slug" (include "dagger-kubernetes.imageCacheSlug" $host) "host" $host "remoteUrl" $remoteUrl "username" (.username | default "") "passwordSecretRef" (.passwordSecretRef | default dict)) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $registries -}}
{{- if hasKey $seen .slug -}}
{{- fail (printf "imageCache: duplicate mirror slug %q (host %q collides with another registry)" .slug .host) -}}
{{- end -}}
{{- $_ := set $seen .slug true -}}
{{- end -}}
{{- $registries | toYaml -}}
{{- end -}}

{{/* Resolve one mirror's in-cluster address. Takes a dict {root, slug}. */}}
{{- define "dagger-kubernetes.imageCacheMirrorAddress" -}}
{{- printf "%s-%s-mirror.%s.svc:5000" .root.Release.Name .slug (include "dagger-kubernetes.namespace" .root) -}}
{{- end -}}

{{/* List the generated mirror addresses (host[:port]) for every enabled
upstream. Empty list when imageCache is disabled. */}}
{{- define "dagger-kubernetes.imageCacheMirrorHosts" -}}
{{- $hosts := list -}}
{{- if .Values.imageCache.enabled -}}
{{- range (fromYamlArray (include "dagger-kubernetes.imageCacheRegistries" .)) -}}
{{- $hosts = append $hosts (include "dagger-kubernetes.imageCacheMirrorAddress" (dict "root" $ "slug" .slug)) -}}
{{- end -}}
{{- end -}}
{{- $hosts | toYaml -}}
{{- end -}}

{{/* Merge the user-provided engine registry mirrors with the generated
image-cache mirror addresses (one per enabled upstream). */}}
{{- define "dagger-kubernetes.engineRegistryMirrors" -}}
{{- $mirrors := deepCopy (.Values.supervisor.config.fleet.engineRegistryMirrors | default dict) -}}
{{- if .Values.imageCache.enabled -}}
{{- range (fromYamlArray (include "dagger-kubernetes.imageCacheRegistries" .)) -}}
{{- $addr := include "dagger-kubernetes.imageCacheMirrorAddress" (dict "root" $ "slug" .slug) -}}
{{- $existing := index $mirrors .host | default list -}}
{{- $mirrors = set $mirrors .host (append $existing $addr) -}}
{{- end -}}
{{- end -}}
{{- $mirrors | toYaml -}}
{{- end -}}

{{/* Resolve the Tempo URL: use the dependency Service when enabled, in the
<service>.<namespace>.svc form (see CONTRIBUTING.md). */}}
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:3200" (include "dagger-kubernetes.tempoServiceName" .) $ns -}}
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
{{- $auto := printf "http://%s.%s.svc:3100" (include "dagger-kubernetes.lokiServiceName" .) $ns -}}
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
{{- $auto := printf "http://%s.%s.svc:8428" (include "dagger-kubernetes.victoriaServerServiceName" .) $ns -}}
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
