# Plan — Fix Helm service URLs for external dependency subcharts (issue #27)

## 1. Context & problem statement

The `dagger-kubernetes` supervisor depends on six Helm subcharts (see
`deploy/helm/dagger-kubernetes/Chart.yaml`): `opentelemetry-collector`
(0.108.0), `minio` (5.4.0), `tempo` (1.24.4), `loki` (7.2.0),
`victoria-metrics-single` (0.44.0, aliased `victoria`), and `grafana`
(10.5.15).

The parent chart "forges" the in-cluster service URLs for those dependencies in
several places by naively concatenating `{{ .Release.Name }}-<fixed-string>`.
This is correct **only when the subchart is left at its default name**. When an
operator sets `fullnameOverride` (or `nameOverride`) on a subchart — e.g.
`tempo.fullnameOverride: my-tempo` — the subchart renders a Service with a
*different* name, but the parent still points the supervisor (and Grafana
datasources and the OTel collector) at `<release>-tempo`, which no longer exists.

The fix: stop hardcoding `{{ .Release.Name }}-<x>` and instead compute each
dependency Service name using that subchart's **own** fullname rules (its
`fullnameOverride` / `nameOverride` handling).

### Where service URLs are forged today

| # | File | Lines | Constructed name (wrong) |
|---|------|-------|--------------------------|
| 1 | `templates/_helpers.tpl` | 40 | `%s-opentelemetry-collector` |
| 2 | `templates/_helpers.tpl` | 65 | `%s-minio` (s3 endpoint) |
| 3 | `templates/_helpers.tpl` | 110 | `%s-minio` (imageCache S3 endpoint) |
| 4 | `templates/_helpers.tpl` | 191 | `%s-tempo` |
| 5 | `templates/_helpers.tpl` | 203 | `%s-loki` |
| 6 | `templates/_helpers.tpl` | 215 | `%s-victoria-server` |
| 7 | `templates/grafana-datasources.yaml` | 16,28,32 | `{{ .Release.Name }}-tempo/-loki/-victoria-server` |
| 8 | `values.yaml` | 809,813,817 | collector exporters `{{ .Release.Name }}-tempo/-loki/-victoria-server` |

(Also `templates/_helpers.tpl` line 158 + `templates/image-cache.yaml` line 6
build the **parent's own** Zot mirror name from `{{ .Release.Name }}` rather than
the parent fullname — related, tracked separately in §10.)

### Critical finding — the VictoriaMetrics name is ALREADY wrong

The vendored `victoria-metrics-single` **0.44.0** chart does **not** render a
Service named `<release>-victoria-server`. It renders the server Service via the
shared helper `vm.plain.fullname` (from the `victoria-metrics-common` library),
which resolves to (verified against the upstream `victoria-metrics-single-0.44.0`
tag):

- `server.fullnameOverride` if set, else
- **legacy naming (default)**: `<release>-victoria-metrics-single-server`, or
- **new naming** (`useLegacyNaming: false`): `vmsingle-<release>`.

`useLegacyNaming` is **not** set in this chart's `values.yaml`, so the default
is legacy → the real Service name is `<release>-victoria-metrics-single-server`.

The current hardcode `{{ .Release.Name }}-victoria-server` uses the **alias**
`victoria` + `-server`, which is inconsistent with the chart's actual output.
This means `telemetry.victoria_url` is very likely **already broken** today
(independent of any `fullnameOverride`), and the fix will *change* the rendered
value. **The Coder agent MUST verify the real Service name with `helm template`
+ `kubectl get svc` (see §8) before/after and confirm against the live cluster.**

## 2. Upstream fullname rules (verified)

The four standard charts share the exact same `contains`-style fullname logic:

```
if .Values.fullnameOverride            -> fullnameOverride | trunc 63 | trimSuffix "-"
else name := default .Chart.Name .Values.nameOverride
     if contains name .Release.Name    -> .Release.Name | trunc 63 | trimSuffix "-"
     else                              -> "<release>-<name>" | trunc 63 | trimSuffix "-"
```

- **tempo** (`tempo.fullname`) → Service = `tempo.fullname` (name `tempo`).
- **opentelemetry-collector** (`opentelemetry-collector.fullname`) → Service name
  = `opentelemetry-collector.fullname` (name `opentelemetry-collector`).
- **minio** (`minio.fullname`) → Service = `minio.fullname` in `mode: standalone`
  (name `minio`).
- **loki** (`loki.singleBinaryFullname`, used in `deploymentMode: SingleBinary`)
  → same `contains` logic; `loki.name` = `ternary "enterprise-logs" "loki"
  .Values.enterprise.enabled` then `tpl .Values.nameOverride $` (name `loki` for
  the default non-enterprise config; note the `tpl`-of-nameOverride nuance).

VictoriaMetrics (`victoria-metrics-single` 0.44.0) is the outlier — its server
Service name follows `vm.plain.fullname` (§1). The helper we add replicates its
legacy branch faithfully and supports `server.fullnameOverride` and
`useLegacyNaming`.

Because the four standard charts use identical logic, we implement **one**
generic helper and thin per-service wrappers, rather than copy four near-duplicate
helpers.

## 3. Design decision

- Reimplement each subchart's fullname rule **in the parent `_helpers.tpl`**
  (Helm cannot call a subchart's `fullname` helper from the parent with the right
  scope: `.Values`/`.Chart.Name` would be the parent's). This is the standard,
  testable approach and exactly satisfies "use the chart's own name function".
- The supervisor ConfigMap and the Grafana datasources are rendered by the
  **parent**, so they auto-fix completely (they read the subchart's
  `fullnameOverride`/`nameOverride` directly).
- The OTel collector config is rendered by the **subchart** via
  `tpl (toYaml $config) .` with the collector's scope, which has **no** access to
  the parent's `.Values.tempo` etc. Helm shares only `global` and `Release`
  across charts, and `global` is static. A full auto-fix there is impossible in
  pure Helm. We therefore route the collector exporters through
  `global.daggerKubernetes.serviceNames.*` (see §6), documented as an escape
  hatch that the operator sets in lock-step with the subchart override.

## 4. File changes

### 4.1 `deploy/helm/dagger-kubernetes/templates/_helpers.tpl`

**Add** the following helpers (place them right after the
`dagger-kubernetes.namespace` helper, before the URL helpers):

```gotmpl
{{/* Compute a dependency subchart's Service name using that subchart's own
fullname rule (the shared "contains" pattern of grafana/tempo, grafana/loki,
minio, and opentelemetry-collector). Args (dict):
  name    - the subchart's Chart.Name (NOT the alias)
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
{{- if .Values.loki.enterprise.enabled -}}{{- $name = "enterprise-logs" -}}{{- end -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" $name "values" .Values.loki "release" .Release.Name) -}}
{{- end -}}

{{- define "dagger-kubernetes.minioServiceName" -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" "minio" "values" .Values.minio "release" .Release.Name) -}}
{{- end -}}

{{- define "dagger-kubernetes.otelCollectorServiceName" -}}
{{- include "dagger-kubernetes.subchartServiceName" (dict "name" "opentelemetry-collector" "values" (index .Values "opentelemetry-collector") "release" .Release.Name) -}}
{{- end -}}

{{/* VictoriaMetrics server Service name (victoria-metrics-single 0.44.0).
Replicates vm.plain.fullname: server.fullnameOverride wins, then the legacy
"<release>-victoria-metrics-single-server" name (default), or "vmsingle-<release>"
when useLegacyNaming is false. */}}
{{- define "dagger-kubernetes.victoriaServerServiceName" -}}
{{- $sub := .Values.victoria | default dict -}}
{{- $server := $sub.server | default dict -}}
{{- if $server.fullnameOverride -}}
{{- $server.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $legacy := ne (toString ($sub.useLegacyNaming | default true)) "false" -}}
{{- if not $legacy -}}
{{- printf "vmsingle-%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $fo := default (($sub.global | default dict).fullnameOverride) $sub.fullnameOverride -}}
{{- if $fo -}}
{{- printf "%s-server" $fo | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "victoria-metrics-single" (default (($sub.global | default dict).nameOverride) $sub.nameOverride) -}}
{{- $base := "" -}}
{{- if contains $name .Release.Name -}}{{- $base = .Release.Name -}}{{- else -}}{{- $base = printf "%s-%s" .Release.Name $name -}}{{- end -}}
{{- printf "%s-server" $base | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
```

**Rewrite** the six URL helpers to use the new names (before → after):

```gotmpl
{{- define "dagger-kubernetes.collectorUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:4318" (include "dagger-kubernetes.otelCollectorServiceName" .) $ns -}}
{{- if index .Values "opentelemetry-collector" "enabled" -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.collectorUrl -}}
{{- end -}}
{{- end -}}
```

```gotmpl
{{- define "dagger-kubernetes.s3Endpoint" -}}
{{- if .Values.supervisor.config.cache.s3.endpoint -}}
{{- .Values.supervisor.config.cache.s3.endpoint -}}
{{- else if .Values.minio.enabled -}}
{{- printf "%s.%s.svc:%v" (include "dagger-kubernetes.minioServiceName" .) (include "dagger-kubernetes.namespace" .) (.Values.minio.service.port | default 9000) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}
```

```gotmpl
{{- define "dagger-kubernetes.imageCacheS3Endpoint" -}}
{{- if .Values.imageCache.storage.s3.endpoint -}}
{{- .Values.imageCache.storage.s3.endpoint -}}
{{- else if .Values.minio.enabled -}}
{{- printf "%s.%s.svc:%v" (include "dagger-kubernetes.minioServiceName" .) (include "dagger-kubernetes.namespace" .) (.Values.minio.service.port | default 9000) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}
```

```gotmpl
{{- define "dagger-kubernetes.tempoUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:3200" (include "dagger-kubernetes.tempoServiceName" .) $ns -}}
{{- if .Values.tempo.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.tempoUrl -}}
{{- end -}}
{{- end -}}
```

```gotmpl
{{- define "dagger-kubernetes.lokiUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:3100" (include "dagger-kubernetes.lokiServiceName" .) $ns -}}
{{- if .Values.loki.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.lokiUrl -}}
{{- end -}}
{{- end -}}
```

```gotmpl
{{- define "dagger-kubernetes.victoriaUrl" -}}
{{- $ns := include "dagger-kubernetes.namespace" . -}}
{{- $auto := printf "http://%s.%s.svc:8428" (include "dagger-kubernetes.victoriaServerServiceName" .) $ns -}}
{{- if .Values.victoria.enabled -}}
{{- $auto -}}
{{- else -}}
{{- default $auto .Values.supervisor.config.telemetry.victoriaUrl -}}
{{- end -}}
{{- end -}}
```

Also **add** the three `global`-scope helpers used by the collector config
(rendered in the collector subchart's scope, where only `.Values.global` and
`.Release` are available):

```gotmpl
{{- define "dagger-kubernetes.otelTempoService" -}}
{{- default (printf "%s-tempo" .Release.Name) (.Values.global.daggerKubernetes.serviceNames.tempo) -}}
{{- end -}}

{{- define "dagger-kubernetes.otelLokiService" -}}
{{- default (printf "%s-loki" .Release.Name) (.Values.global.daggerKubernetes.serviceNames.loki) -}}
{{- end -}}

{{- define "dagger-kubernetes.otelVictoriaService" -}}
{{- default (printf "%s-victoria-metrics-single-server" .Release.Name) (.Values.global.daggerKubernetes.serviceNames.victoria) -}}
{{- end -}}
```

> Note: `dagger-kubernetes.otelVictoriaService`'s default is
> `<release>-victoria-metrics-single-server` (the real 0.44.0 name), **not** the
> old `<release>-victoria-server`.

### 4.2 `deploy/helm/dagger-kubernetes/templates/grafana-datasources.yaml`

Replace the three hardcoded URLs with the helpers (before → after):

```yaml
        url: "http://{{ include "dagger-kubernetes.tempoServiceName" . }}.{{ include "dagger-kubernetes.namespace" . }}.svc:3200"
```
```yaml
        url: "http://{{ include "dagger-kubernetes.lokiServiceName" . }}.{{ include "dagger-kubernetes.namespace" . }}.svc:3100"
```
```yaml
        url: "http://{{ include "dagger-kubernetes.victoriaServerServiceName" . }}.{{ include "dagger-kubernetes.namespace" . }}.svc:8428"
```

### 4.3 `deploy/helm/dagger-kubernetes/values.yaml`

**(a)** Add a `global` block carrying the collector's service-name overrides
(empty default = auto-compute):

```yaml
## @section Cross-subchart service names (collector exporters)
## @descriptionStart
## The OTel collector config is rendered by the opentelemetry-collector
## subchart, whose template scope cannot see the parent's `tempo`/`loki`/
## `victoria` values. To point the collector's exporters at a renamed backend,
## set the matching value here *in addition to* the subchart's own
## `fullnameOverride`. Empty = auto (`<release>-<default-name>`).
## @descriptionEnd
global:
  daggerKubernetes:
    serviceNames:
      tempo: ""
      loki: ""
      victoria: ""
```

**(b)** Replace the three collector exporter endpoints:

```yaml
    exporters:
      otlphttp/tempo:
        endpoint: http://{{ include "dagger-kubernetes.otelTempoService" . }}.{{ .Release.Namespace }}.svc:4318
        tls:
          insecure: true
      loki:
        endpoint: http://{{ include "dagger-kubernetes.otelLokiService" . }}.{{ .Release.Namespace }}.svc:3100/loki/api/v1/push
        tls:
          insecure: true
      prometheusremotewrite:
        endpoint: http://{{ include "dagger-kubernetes.otelVictoriaService" . }}.{{ .Release.Namespace }}.svc:8428/prometheus/api/v1/write
        tls:
          insecure: true
```

> `.Release.Namespace` is kept (not `include "dagger-kubernetes.namespace"`)
> because the collector must reach the subcharts, which deploy in the release
> namespace.

**(c)** Update the `@param` doc comments for the telemetry URL values (lines
~220-229) and the s3 endpoint (~232) so they no longer claim a fixed
`<release>-<svc>` name; describe the subchart-fullname-derived name instead.

### 4.4 `deploy/helm/dagger-kubernetes/README.md`

- Update the "Auto-wiring" table (§ "Auto-wiring", ~line 448-459) so the
  `telemetry.victoriaUrl` row reads `<release>-victoria-metrics-single-server`
  (default) and note that all auto-wired names follow each subchart's
  `fullnameOverride`/`nameOverride`.
- Update the `supervisor.config.telemetry.victoriaUrl` parameter doc (~line 676).
- Document the new `global.daggerKubernetes.serviceNames.*` keys (§ "Chart
  metadata" or a new subsection) and the lock-step requirement with the
  collector.

### 4.5 `docs/README.md`

- Update the "Default URLs (auto-wired by Helm)" table (lines 1640-1643):
  change the VictoriaMetrics row to `<release>-victoria-metrics-single-server`
  and add a sentence that all names are derived from each subchart's fullname
  (honoring `fullnameOverride`/`nameOverride`).

### 4.6 `dagger/main.go` (CI matrix — optional but recommended)

Add matrix entries that exercise subchart renames so CI proves the fix:

```go
{
    "--set", "tempo.fullnameOverride=my-tempo",
    "--set", "loki.fullnameOverride=my-loki",
    "--set", "victoria.server.fullnameOverride=my-victoria",
    "--set", "minio.fullnameOverride=my-minio",
    "--set", "opentelemetry-collector.fullnameOverride=my-otel",
},
```

Because this touches `dagger/`, also update `DAGGER.md` (function table note that
the template matrix now includes a subchart-`fullnameOverride` variant).

## 5. Go code / config structs — NO changes

The config **keys** (`telemetry.victoria_url`, `telemetry.tempo_url`,
`telemetry.loki_url`, `telemetry.collector_url`, `cache.s3.endpoint`) are
unchanged; only the Helm-rendered **values** change. Therefore:

- `internal/domain/config.go` (`VictoriaURL` etc.) — unchanged.
- `config/loader.go` (`v.SetDefault("telemetry.victoria_url", "http://victoria:8428")`)
  — unchanged (it is the non-Helm dev/compose default).
- `config/config.app.yaml.sample` — unchanged (the sample's bare names are the
  documented non-K8s fallbacks; the chart overrides them with `.svc` forms).

## 6. Edge cases

- **`fullnameOverride` on a standard subchart** — helper returns it (trunc 63,
  trim `-`). Supervisor configmap + Grafana datasources follow automatically.
- **`nameOverride` on a standard subchart** — helper uses `default name nameOverride`.
- **`contains name .Release.Name`** — if the release name already contains the
  subchart name, the subchart uses the bare release name; the helper replicates
  this exactly.
- **Empty release name** (`helm template .` with no release) — `printf "%s-%s"`
  would produce a leading `-`; this is the same behavior as the subcharts
  themselves (they also `printf "%s-%s"`), so it stays consistent. Not a real
  install case.
- **Subchart disabled** (`tempo.enabled: false`, etc.) — the URL helpers fall back
  to the operator's explicit `supervisor.config.telemetry.*Url` override, then to
  the auto name; unchanged semantics.
- **Alias changes** — the dependency names are fixed in `Chart.yaml`; the helpers
  hardcode the subchart `Chart.Name` (e.g. `victoria-metrics-single`), which is
  what matters (an alias never changes `.Chart.Name` inside the subchart).
- **Port differences** — `minio.service.port` (9000) is honored; the other ports
  (4318/3200/3100/8428) are fixed by the subcharts and kept as-is.
- **Namespace differences** — supervisor/collector URLs use the parent
  `namespace` override / release namespace respectively, matching where each
  subchart actually deploys (see §4.3 note).
- **`victoria.server.fullnameOverride`** vs **`victoria.fullnameOverride`** vs
  **`victoria.useLegacyNaming`** — handled in `victoriaServerServiceName`
  (server-level override wins; legacy default; `vmsingle-` prefix when
  `useLegacyNaming: false`).
- **Grafana datasources** — only rendered when `grafana.enabled`; the URL helper
  is resolved at render time so `fullnameOverride` is honored.
- **Collector config** — the `global.daggerKubernetes.serviceNames.*` escape hatch
  must be kept in sync with the subchart override by the operator (documented);
  this is the one place a rename is **not** auto-derived.

## 7. Error handling & validation

- `helm lint .` must pass (no undefined helpers, no bad indentation).
- `helm template` for every existing `helmTemplateMatrix` combo must still
  succeed (see `dagger/main.go`).
- The `global.daggerKubernetes.serviceNames.*` values must default to `""`
  (auto). The `dagger-kubernetes.otel*Service` helpers use `default` so a missing
  global key cannot fail the render.
- No new `required`/`fail` guards are needed (all inputs have safe defaults).

## 8. Testing / verification strategy

```bash
cd deploy/helm/dagger-kubernetes
helm dependency build            # charts/*.tgz are gitignored; fetch first

# Baseline
helm lint .
helm template dagger-kubernetes . --debug > /tmp/default.yaml

# With subchart renames
helm template dagger-kubernetes . --debug \
  --set tempo.fullnameOverride=my-tempo \
  --set loki.fullnameOverride=my-loki \
  --set victoria.server.fullnameOverride=my-victoria \
  --set minio.fullnameOverride=my-minio \
  --set opentelemetry-collector.fullnameOverride=my-otel \
  > /tmp/overrides.yaml

# 1. Rendered supervisor URLs
grep -E 'collector_url|tempo_url|loki_url|victoria_url|endpoint:' /tmp/default.yaml /tmp/overrides.yaml

# 2. Rendered Service names from the subcharts (ground truth to diff against)
grep -E 'kind: Service' -A2 /tmp/overrides.yaml | grep 'name:'

# Expected (default): tempo=...-tempo, loki=...-loki,
#   minio=...-minio, otel=...-opentelemetry-collector,
#   victoria=...-victoria-metrics-single-server
# Expected (overrides): my-tempo / my-loki / my-victoria / my-minio / my-otel
```

**Mandatory live-cluster check (VictoriaMetrics):**

```bash
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test get svc | grep -i victoria
```

Confirm the actual Service name matches `dagger-kubernetes-test-victoria-metrics-single-server`
(or `vmsingle-...` if `useLegacyNaming:false`), **not** the old
`dagger-kubernetes-test-victoria-server`. Because this fix changes the rendered
`victoria_url`, after deployment verify `GET /api/v1/status` still reports
VictoriaMetrics healthy and `GET /api/v1/traces/:id/metrics` still returns data
(see `AGENTS.local.md` §5).

**CI gate:** run the full gate `dagger call -m ./dagger --src . ci export --path out`
(which runs `helm lint` + the template matrix). Minimum when Docker is
unavailable: `go build ./... && go vet ./... && go test ./...` plus
`dagger call -m ./dagger --src . lint` — but the Helm part of this change is only
fully validated by the template matrix, so prefer the full gate.

## 9. Ordered implementation steps (for the Coder agent)

1. Edit `templates/_helpers.tpl`: add the generic + per-service helpers, rewrite
   the six URL helpers, add the three `global`-scope `otel*Service` helpers.
2. Edit `templates/grafana-datasources.yaml`: swap the three URLs to the helpers.
3. Edit `values.yaml`: add the `global.daggerKubernetes.serviceNames.*` block,
   swap the three collector exporter endpoints, and update the `@param` comments
   for the telemetry/s3 URL values.
4. Run `helm dependency build` then `helm lint .` and the `helm template`
   matrix in §8; fix any render errors.
5. Verify rendered URLs against rendered Service names (§8).
6. Update `deploy/helm/dagger-kubernetes/README.md` (auto-wiring table, parameter
   docs, new `global` keys).
7. Update `docs/README.md` (default-URLs table + fullname note).
8. (Recommended) Add the subchart-`fullnameOverride` variant to
   `dagger/main.go` `helmTemplateMatrix` and update `DAGGER.md`.
9. Run the CI gate; redeploy to the `home` cluster per `AGENTS.local.md` §4 and
   validate per §5 (this changes the live `victoria_url`, so the VictoriaMetrics
   service name + status/metrics checks are mandatory).

## 10. Related but out-of-scope (note for future)

- The parent's **own** Zot image-cache mirror name
  (`dagger-kubernetes.imageCacheMirrorAddress` / `image-cache.yaml`) is built
  from `{{ .Release.Name }}` rather than `{{ include "dagger-kubernetes.fullname" . }}`,
  so it ignores the **parent's** `fullnameOverride`. This is a separate bug from
  issue #27 (subchart renames) and can be fixed in a follow-up.

## 11. Open questions / risks

- **VictoriaMetrics default naming** (legacy vs `useLegacyNaming`) is the one
  item that must be confirmed empirically against the vendored `victoria-metrics-single-0.44.0.tgz`
  and the live cluster before merging (§8). If the live cluster proves the old
  `-victoria-server` name is real, re-open this plan's §4.1 victoria helper.
- **Collector fullnameOverride auto-derivation** is impossible in pure Helm; the
  `global` escape hatch is a documented limitation. If auto-derivation is
  required later, the alternative is to have the parent render the collector's
  ConfigMap itself (breaks the collector's `presets` and config-checksum
  auto-restart) — deferred.
