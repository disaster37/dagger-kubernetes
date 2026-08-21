# Fleet `version_retention` — idle engine-fleet garbage collection

## 1. Goal & behavior spec

Automatically delete a Dagger-engine **version** (its StatefulSet **and** its
headless Service) after it has been idle for a configurable duration
`fleet.version_retention`. Today `Manager.Sweep`/`sweepVersion` only scale idle
replicas down to 0 (`replica_idle_ttl`, default `5m`); a version's StatefulSet
and Service accumulate forever, retaining their PVCs (StatefulSet PVC retention
policy is `WhenScaled: Retain`, so scale-to-0 keeps the disk). This change adds
the missing GC that the ADRs previously promised but never implemented.

"Idle" means: **zero replicas AND zero pinned sessions for that version**.

Idle state is persisted durably across supervisor restarts **without a new Raft
FSM table** — it is stored as an annotation on the engine StatefulSet:

- Key: `dagger-kubernetes.io/idle-since`
- Value: RFC3339 UTC timestamp, e.g. `2026-08-21T12:00:00Z`
  (`idleSince.UTC().Format(time.RFC3339)`).
- The sweep loop is the **only writer** of this annotation. When the version
  becomes active again (replicas > 0 **or** pinned sessions exist), the
  annotation is cleared so the idle clock restarts when it truly idles.

GC deletes the **Service first, then the StatefulSet**. PVCs are removed by the
existing StatefulSet `PersistentVolumeClaimRetentionPolicy{WhenDeleted: Delete}`
policy (verified in `buildStatefulSet`). Deleting the Service first makes the
operation self-healing: if the StatefulSet delete fails after the Service is
gone, the next sweep (which lists versions via `AllVersions` = list of STSs)
still sees the STS and retries; deleting the STS first would orphan the Service
forever (the version would disappear from `AllVersions`).

### Semantics table (per version, evaluated every 30s sweep)

| Condition                                                                     | Action                                                                                                    |
|-------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| `version_retention <= 0` (feature disabled)                                   | Run the existing scale-down loop **only**. No idle-since reads/writes, no GC.                             |
| `replicas > 0` (any)                                                          | Existing scale-down loop runs (unchanged). After it, if an `idle-since` annotation exists, **clear it**.  |
| `replicas == 0` **and** pinned sessions exist (stale sessions on a dead version) | **Skip GC** (conservative). **Clear** `idle-since` (the version is not fully idle).                     |
| `replicas == 0`, no pinned sessions, no `idle-since` annotation               | **Stamp** `idle-since = now` (UTC). No delete this cycle.                                                 |
| `replicas == 0`, no pinned sessions, `idle-since` set, `now - idleSince < retention` | No-op (still waiting).                                                                              |
| `replicas == 0`, no pinned sessions, `idle-since` set, `now - idleSince >= retention` | **GC**: `DeleteService(version)` then `DeleteStatefulSet(version)`. PVCs removed by STS retention policy. |
| `idle-since` annotation value is malformed (not RFC3339)                      | Log WARN + **reset** `idle-since = now`. No delete this cycle. Never crash the sweep.                     |
| `DeleteService` / `DeleteStatefulSet` returns an error                        | Log + return error to the per-version sweep wrapper (already logs "sweep version error"). Next sweep retries. Never crash the process. |

Edge cases:

- **First idle observation**: an STS created but never used (0 replicas) is
  stamped on the first sweep and GC'd after `version_retention` — intended.
- **Active-again**: a client acquires a version that had been idle; the sweep
  clears the annotation once replicas > 0 (Acquire itself does not write the
  annotation — sweep remains the only writer).
- **Race (documented, accepted)**: between the sweep's `GetReplicas()==0` read
  and the delete, a concurrent `Acquire` could scale the version back up. The
  window is one sweep tick; the default 24h retention makes it vanishingly rare,
  and a fresh `Acquire` re-creates the STS+Service anyway (`EnsureStatefulSet` /
  `EnsureService` are idempotent). Not adding extra locking.
- **`cache.gc.protect_active_versions` interaction** (`internal/service/cache_stats.go`):
  `activeVersions()` lists fleet versions via the provider and protects cache
  tags for versions with ready replicas. After this feature deletes an idle
  version's STS, that version's tags become unprotected and may be purged by the
  cache GC on a later run. This is the **intended** ADR-012 interaction — document it.

## 2. Committed design decisions

1. **Two interface methods** (not a single combined method) on
   `domain.FleetProvider`:

   ```go
   VersionIdleSince(version string) (time.Time, bool, error)
   SetVersionIdleSince(version string, idleSince time.Time) error
   ```

   Rationale: read and write have distinct callers and error semantics; a single
   `(idleSince *time.Time)`-style overload is less idiomatic and harder to stub.
   Contract for `VersionIdleSince`:
   - STS missing OR annotation absent → `(time.Time{}, false, nil)`.
   - annotation present + valid → `(t, true, nil)`.
   - annotation present + malformed → `(time.Time{}, true, error)` (parse error wrapped).
   - read error → `(time.Time{}, false, error)`.
   `SetVersionIdleSince(version, time.Time{})` **clears** the annotation
   (deletes the key). The service disambiguates "malformed" (ok==true, err!=nil)
   from "read failed" (ok==false, err!=nil).

2. **Annotation** `dagger-kubernetes.io/idle-since`, RFC3339 UTC, on the engine
   STS `ObjectMeta.Annotations`. Constant `engineIdleSinceAnnotation` in
   `internal/repository/k8s_provider.go`.

3. **Config plumbing** (snake_case app key → camelCase Helm key):
   - `internal/domain/config.go`: `FleetConfig.VersionRetention time.Duration`
     with `mapstructure:"version_retention"`.
   - `config/loader.go`: `v.SetDefault("fleet.version_retention", 24*time.Hour)`.
   - Env var: `DAGGER_KUBERNETES_FLEET_VERSION_RETENTION` (automatic via
     `SetEnvPrefix` + `.`→`_` replacer; no code change).
   - `<= 0` ⇒ feature disabled (never GC).

4. **Helm**: `supervisor.config.fleet.versionRetention` (string `"24h"`) rendered
   into the configmap as `version_retention: "24h"` (quoted, same convention as
   `replica_idle_ttl`).

5. **Sweep ordering** in `sweepVersion`: existing scale-down loop first
   (byte-for-byte unchanged), then the GC block gated on `len(replicas) == 0`.
   Pinned-session check uses `m.sessions.List()` filtering `Lease.Version == version`
   (the `domain.SessionStore` interface already exposes `List() []*Lease`, and
   `Lease.Version` is already populated by `Store.Register`).

## 3. File-by-file changes

### 3.1 `internal/domain/fleet.go`
Add two methods to `FleetProvider` (after `AllVersions()`):

```go
	// VersionIdleSince returns the RFC3339 UTC time the version was first
	// observed idle (zero replicas, no pinned sessions), or ok=false when the
	// StatefulSet is missing or carries no idle-since annotation. A malformed
	// annotation returns ok=true with a parse error.
	VersionIdleSince(version string) (time.Time, bool, error)
	// SetVersionIdleSince stamps (or, when idleSince is the zero time, clears)
	// the idle-since annotation on the version's StatefulSet.
	SetVersionIdleSince(version string, idleSince time.Time) error
```

No other domain changes.

### 3.2 `internal/domain/config.go`
In `FleetConfig` (after `ReplicaIdleTTL`, line ~243):

```go
	ReplicaIdleTTL         time.Duration           `mapstructure:"replica_idle_ttl"`
	VersionRetention       time.Duration           `mapstructure:"version_retention"`
```

### 3.3 `config/loader.go`
After line 117 (`v.SetDefault("fleet.replica_idle_ttl", 5*time.Minute)`):

```go
	v.SetDefault("fleet.version_retention", 24*time.Hour)
```

No validator changes needed (`<= 0` is a valid "disabled" value handled in the
sweep).

### 3.4 `cmd/api/main.go`
In the `service.NewManager(...)` call (lines 232–236), add:

```go
		VersionRetention:       cfg.Fleet.VersionRetention,
```

### 3.5 `internal/service/fleet.go`
- Add field to `Manager`:

  ```go
  	replicaIdleTTL        time.Duration
  	versionRetention      time.Duration
  ```

- Add field to `ManagerConfig`:

  ```go
  	ReplicaIdleTTL        time.Duration
  	VersionRetention      time.Duration
  ```

- In `NewManager`, assign `versionRetention: cfg.VersionRetention`.

- Rewrite `sweepVersion` to keep the existing scale-down loop and append the GC
  block. Reference implementation:

```go
func (m *Manager) sweepVersion(version string) error {
	replicas, err := m.provider.GetReplicas(version)
	if err != nil {
		return fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(version, replicas)

	sortDescendingOrdinal(replicas)

	for _, r := range replicas {
		if m.replicaHasActiveSessions(r.Name) {
			continue
		}
		idle := time.Since(r.StartedAt)
		if idle < m.replicaIdleTTL {
			continue
		}

		m.logger.WithFields(logrus.Fields{
			"version": version,
			"pod":     r.Name,
			"idle":    idle,
		}).Info("scaling down idle replica")

		if err := m.provider.ScaleDown(version, r.Ordinal); err != nil {
			return fmt.Errorf("scale down %s: %w", r.Name, err)
		}
		break
	}

	if len(replicas) > 0 {
		// Version is active (has pods); clear any stale idle-since stamp.
		return m.clearIdleSinceIfPresent(version)
	}

	// Zero replicas: run idle tracking + GC.
	return m.gcIdleVersion(version)
}

// clearIdleSinceIfPresent removes the idle-since annotation when it exists.
func (m *Manager) clearIdleSinceIfPresent(version string) error {
	_, ok, err := m.provider.VersionIdleSince(version)
	if err != nil {
		return fmt.Errorf("get idle since: %w", err)
	}
	if !ok {
		return nil
	}
	if err := m.provider.SetVersionIdleSince(version, time.Time{}); err != nil {
		return fmt.Errorf("clear idle since: %w", err)
	}
	return nil
}

// gcIdleVersion evaluates idle-since tracking and, once version_retention
// elapses, deletes the idle version's Service then StatefulSet.
func (m *Manager) gcIdleVersion(version string) error {
	if m.versionRetention <= 0 {
		return nil // feature disabled
	}

	if m.versionHasPinnedSessions(version) {
		// Stale sessions still pin this (dead) version; be conservative and
		// restart the idle clock so GC does not fire prematurely once the
		// sessions are reaped.
		return m.clearIdleSinceIfPresent(version)
	}

	idleSince, ok, err := m.provider.VersionIdleSince(version)
	if err != nil {
		if ok {
			// Malformed annotation: log + reset to now, do not crash.
			m.logger.WithFields(logrus.Fields{
				"version": version,
			}).WithError(err).Warn("resetting malformed idle-since annotation")
			if setErr := m.provider.SetVersionIdleSince(version, time.Now()); setErr != nil {
				return fmt.Errorf("reset idle since: %w", setErr)
			}
			return nil
		}
		return fmt.Errorf("get idle since: %w", err)
	}

	now := time.Now()
	if !ok {
		// First idle observation: stamp and wait for version_retention.
		if err := m.provider.SetVersionIdleSince(version, now); err != nil {
			return fmt.Errorf("set idle since: %w", err)
		}
		return nil
	}

	if now.Sub(idleSince) < m.versionRetention {
		return nil
	}

	// Delete the Service first (self-healing: if the STS delete fails, the
	// next sweep still lists the STS and retries), then the StatefulSet.
	if err := m.provider.DeleteService(version); err != nil {
		return fmt.Errorf("delete service %s: %w", version, err)
	}
	if err := m.provider.DeleteStatefulSet(version); err != nil {
		return fmt.Errorf("delete statefulset %s: %w", version, err)
	}
	m.logger.WithField("version", version).Info("deleted idle engine fleet version")
	return nil
}

// versionHasPinnedSessions reports whether any session still pins a replica of
// the given version.
func (m *Manager) versionHasPinnedSessions(version string) bool {
	for _, l := range m.sessions.List() {
		if l.Version == version {
			return true
		}
	}
	return false
}
```

Note: `time` is already imported; `fmt` and `logrus` are already imported.

### 3.6 `internal/repository/k8s_provider.go`
- Add a constant next to the other engine constants (line ~24–40):

  ```go
  	engineIdleSinceAnnotation = "dagger-kubernetes.io/idle-since"
  ```

- Add two methods (anywhere near `DeleteService`/`AllVersions`):

```go
// VersionIdleSince returns the idle-since annotation value. ok is true when the
// annotation exists (including a malformed value, which returns ok=true plus a
// parse error); ok is false when the StatefulSet is missing or has no annotation.
func (p *K8sProvider) VersionIdleSince(version string) (time.Time, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	name := domain.StsName(version)
	sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("get statefulset %s: %w", name, err)
	}

	raw, ok := sts.Annotations[engineIdleSinceAnnotation]
	if !ok || raw == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("parse idle-since annotation %q: %w", raw, err)
	}
	return t, true, nil
}

// SetVersionIdleSince writes (or, when idleSince is zero, clears) the
// idle-since annotation. A missing StatefulSet is a no-op (already deleted).
func (p *K8sProvider) SetVersionIdleSince(version string, idleSince time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.StsName(version)
	sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get statefulset %s: %w", name, err)
	}

	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	if idleSince.IsZero() {
		delete(sts.Annotations, engineIdleSinceAnnotation)
	} else {
		sts.Annotations[engineIdleSinceAnnotation] = idleSince.UTC().Format(time.RFC3339)
	}

	if _, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update statefulset %s: %w", name, err)
	}
	return nil
}
```

(`metav1`, `apierrors`, `fmt`, `time` are already imported.)

### 3.7 `internal/repository/stub_provider.go`
- Add `idleSince time.Time` to `stubSTS`:

  ```go
  type stubSTS struct {
  	replicasM map[string]*domain.Replica
  	nextIP    int
  	idleSince time.Time // zero = unset
  }
  ```

- Add the two methods:

```go
func (p *StubProvider) VersionIdleSince(version string) (time.Time, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok || sts.idleSince.IsZero() {
		return time.Time{}, false, nil
	}
	return sts.idleSince, true, nil
}

func (p *StubProvider) SetVersionIdleSince(version string, idleSince time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok {
		return nil // version already gone
	}
	sts.idleSince = idleSince
	return nil
}
```

(`time` is already imported.)

### 3.8 `internal/service/cache_stats_test.go` — `stubFleetProvider` (lines ~163–183)
Add the two methods (compile fix for the interface change). `time` is already
imported in this file:

```go
func (p *stubFleetProvider) VersionIdleSince(string) (time.Time, bool, error) { return time.Time{}, false, nil }
func (p *stubFleetProvider) SetVersionIdleSince(string, time.Time) error       { return nil }
```

(`status_test.go` uses this same stub, so no separate change there.)

## 4. Tests

Conventions: stdlib `testing` only, table-driven, fake clientset for K8s,
`observ.NewTestLogger()` (or `logrus.New()` + `io.Discard`), 100% coverage for
touched packages.

### 4.1 `internal/repository/k8s_provider_test.go` — add
- `TestK8sVersionIdleSinceAbsent` — `EnsureStatefulSet`; `VersionIdleSince`
  → `(zero, false, nil)`.
- `TestK8sSetAndGetVersionIdleSince` — `SetVersionIdleSince(v, t)` with a
  non-UTC `t`; `VersionIdleSince` → `(t.UTC(), true, nil)`; assert the STS
  annotation equals `t.UTC().Format(time.RFC3339)`.
- `TestK8sClearVersionIdleSince` — set then `SetVersionIdleSince(v, time.Time{})`;
  `VersionIdleSince` → `(zero, false, nil)`; annotation key absent from STS.
- `TestK8sVersionIdleSinceMalformed` — `EnsureStatefulSet`, then directly write
  `sts.Annotations[engineIdleSinceAnnotation] = "not-a-time"` via the fake
  clientset; `VersionIdleSince` → `(zero, true, non-nil error)`.
- `TestK8sVersionIdleSinceMissingSTS` — `VersionIdleSince("v9.9.9")` →
  `(zero, false, nil)`.
- `TestK8sSetVersionIdleSinceMissingSTSNoop` — `SetVersionIdleSince("v9.9.9", t)`
  → `nil`, no error.
- `TestK8sSetVersionIdleSincePreservesLabels` — after set/clear, assert
  `app=dagger-engine` / `version=<v>` labels are intact.

### 4.2 `internal/service/fleet_test.go` — add (use `repository.NewStubProvider()`)
Deterministic expiry via `provider.SetVersionIdleSince(v, time.Now().Add(-time.Hour))`
(no wall-clock sleep):

- `TestSweepGCDisabled` — `VersionRetention: 0`, ensure STS, zero replicas;
  `Sweep`; assert no idle-since stamped (`VersionIdleSince` ok=false) and
  version still present in `AllVersions()`.
- `TestSweepGCStampsFirstThenDeletes` — `VersionRetention: 30*time.Minute`;
  ensure STS; `Sweep` → idle-since stamped (ok=true), STS still present;
  pre-set idle-since to `now-1h`; `Sweep` → version gone from `AllVersions()`,
  `GetReplicas` returns empty.
- `TestSweepGCSkipsWhenPinnedSessionsExist` — zero replicas, pre-set idle-since
  to `now-1h`, register a session with `sessions.Register("fp","v0.21.4","pod-0",...)`;
  `Sweep` → version NOT deleted and idle-since cleared (ok=false).
- `TestSweepGCClearsIdleSinceWhenActive` — zero replicas, pre-set idle-since,
  then `ScaleUp` to 1; `Sweep` → version NOT deleted, idle-since cleared.
- `TestSweepGCMalformedAnnotationResets` — define a local wrapper embedding
  `*repository.StubProvider` whose `VersionIdleSince` returns
  `(time.Time{}, true, errors.New("parse ..."))`; `VersionRetention: 30m`;
  `Sweep` → no delete, and `SetVersionIdleSince` was called with a non-zero time
  (record via the wrapper).
- `TestSweepGCDeletesServiceThenStatefulSet` — a recording wrapper embedding
  `*repository.StubProvider` that appends method names to a slice; pre-set
  expiry; `Sweep`; assert the recorded order is `["DeleteService",
  "DeleteStatefulSet"]`.

### 4.3 `internal/service/k8s_manager_test.go` — add (fake clientset)
- `TestK8sManagerSweepGCDeletesIdleVersion` — fake clientset + `K8sProvider`;
  `EnsureStatefulSet` + `EnsureService`; `SetVersionIdleSince(v, now-1h)`;
  `ManagerConfig.VersionRetention: 30*time.Minute`; `Sweep`; assert both the STS
  and Service are gone from the fake clientset (`IsNotFound`).
- `TestK8sManagerSweepGCMalformedAnnotationResets` — set the annotation to
  `"garbage"` directly; `Sweep`; assert STS still present and annotation now
  parses to a time within a few seconds of `now`.

### 4.4 `config/loader_test.go` — extend
- `TestLoadDefaults` (line 13): add
  `if cfg.Fleet.VersionRetention != 24*time.Hour { t.Fatalf(...) }`.
- `TestLoadFile` (line 165): add a `fleet.version_retention: "2h"` override in
  the temp config and assert `cfg.Fleet.VersionRetention == 2*time.Hour`.
- `TestLoadEnvOverride` (line 247): assert
  `DAGGER_KUBERNETES_FLEET_VERSION_RETENTION=90m` overrides the default
  (mirror an existing env-override case).

### 4.5 Existing tests that break (compile) and the fix
The `domain.FleetProvider` interface gains two methods, so **every** implementer
must add them or fail to compile:
- `internal/repository/k8s_provider.go` — added above.
- `internal/repository/stub_provider.go` — added above.
- `internal/service/cache_stats_test.go` `stubFleetProvider` — added above.
- `internal/handler/server_test.go` `faultyProvider` (line ~417 embeds
  `*repository.StubProvider`) and `internal/handler/data_conn_test.go` /
  `test_helper_test.go` — these embed or wrap `repository.StubProvider`, so they
  inherit the new methods automatically; **no change** unless they define their
  own `FleetProvider` type. Verify by compiling the `internal/handler` package.

No `ManagerConfig{...}` literal breaks (all use named fields; adding a field is
backward-compatible). `tests/integration/*` and
`internal/repository/k8s_provider_integration_test.go` need no change.

## 5. Docs & ADR edits (part of the changeset)

### 5.1 `config/config.app.yaml.sample`
After line 203 (`replica_idle_ttl: "5m" ...`), add:

```yaml
  version_retention: "24h"                   # idle version GC: delete a version's StatefulSet+Service after this long with zero replicas and no pinned sessions (<= 0 disables).
```

### 5.2 `docs/README.md`
- Fleet config table (~line 430, after `replica_idle_ttl` row): add

  ```
  |                 | `version_retention`                       | `24h`                                                    | Idle version GC: delete a version's StatefulSet + Service after this long with zero replicas and no pinned sessions (`<= 0` disables). |
  ```

- "Engine fleet" section (~line 497): after the scale-down sentence, append a
  sentence describing GC, e.g. "A version that has zero replicas and no pinned
  sessions for `version_retention` (default `24h`; `<= 0` disables) is garbage
  collected: the supervisor deletes its Service and StatefulSet (the StatefulSet
  PVC retention policy removes its disk)."

- Tuning guidance (~line 1349–1351): add
  `fleet.version_retention` (idle-version GC horizon) to the autoscaler tuning
  list.

### 5.3 `deploy/helm/dagger-kubernetes/values.yaml`
- After line 146 (`## @param supervisor.config.fleet.replicaIdleTtl ...`), add:

  ```
  ## @param supervisor.config.fleet.versionRetention Idle version GC: delete a version's StatefulSet + Service after this long with zero replicas and no pinned sessions (<= 0 disables).
  ```

- Under `fleet:` after `replicaIdleTtl: "5m"` (line 184), add:

  ```yaml
        versionRetention: "24h"
  ```

### 5.4 `deploy/helm/dagger-kubernetes/templates/configmap.yaml`
After line 118 (`replica_idle_ttl: {{ ... | quote }}`), add:

```yaml
      version_retention: {{ .Values.supervisor.config.fleet.versionRetention | quote }}
```

### 5.5 `deploy/helm/dagger-kubernetes/README.md`
In the "Supervisor configuration" Parameters table (after the `replicaIdleTtl`
row, line 585), add:

```
| `supervisor.config.fleet.versionRetention` | string | `"24h"` | Idle version GC: delete a version's StatefulSet + Service after this long with zero replicas and no pinned sessions (`<= 0` disables). |
```

### 5.6 `docs/design/ADR-004-per-version-statefulset-autoscaler.md`
Replace the "Superseded (2026-08-20)" blockquote (lines 26–30) with:

```markdown
> **Implemented (2026-08-21):** the `version_retention` garbage-collection
> behavior above is now implemented. `Sweep` deletes a version's StatefulSet
> (and its headless Service) once it has had zero replicas and no pinned
> sessions for `fleet.version_retention` (default `24h`; `<= 0` disables GC).
> "Idle since" is tracked durably as the `dagger-kubernetes.io/idle-since`
> annotation (RFC3339, UTC) on the engine StatefulSet — no Raft FSM table is
> added. See `internal/service/fleet.go`.
```

### 5.7 `docs/design/ADR-012-magiccache-dashboard.md`
Replace the "Superseded (2026-08-20)" blockquote (lines 98–100) with:

```markdown
  > **Implemented (2026-08-21):** `fleet.version_retention` is now implemented.
  > When it deletes an idle version's StatefulSet (and Service), that version's
  > cache tags become unprotected (no active replicas) and may be purged by the
  > cache GC on its next run — the intended interaction with
  > `cache.gc.protect_active_versions`.
```

## 6. Build, lint, Helm, and live-cluster redeploy (MANDATORY — AGENTS.local.md §6)

### 6.1 Pre-flight (local)
```bash
gofmt -w ./internal/domain/fleet.go ./internal/domain/config.go \
  ./config/loader.go ./cmd/api/main.go \
  ./internal/service/fleet.go \
  ./internal/repository/k8s_provider.go ./internal/repository/stub_provider.go
goimports -w -local github.com/disaster/dagger-kubernetes <same files + edited tests>
go build ./...
go test ./internal/domain/... ./internal/service/... ./internal/repository/... ./config/... ./cmd/api/...
helm lint ./deploy/helm/dagger-kubernetes
```

### 6.2 Build + push image
```bash
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev
```

### 6.3 Capture live values (strip stale keys)
```bash
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml
```
If the captured file contains `supervisor.config.raft.replicas` or
`supervisor.autoscaling`, remove those keys (the chart fails closed on them).
Add the new key to the captured file (default is chart-provided, but explicit is
safer for the temporary E2E below):
```yaml
supervisor:
  config:
    fleet:
      versionRetention: "24h"
```

### 6.4 Upgrade
```bash
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes \
  --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
```

### 6.5 Force rollout + wait
```bash
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

### 6.6 Agent verification (must pass)
```bash
# 1. Pods Ready
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  get pods -l app.kubernetes.io/name=dagger-kubernetes

# 2. Probes (control plane serves HTTPS)
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  port-forward svc/dagger-kubernetes-test-dagger-kubernetes-control 8080:80 &
curl -sk https://localhost:8080/healthz   # expect 200 ok
curl -sk https://localhost:8080/readyz    # expect 200

# 3. Authed API smoke via login cookie (no curl in the image)
curl -sk -c /tmp/cookies.txt -X POST https://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"DaggerHome!2026"}'
ACCESS=$(grep -o 'dagger_kubernetes_access[^;]*' /tmp/cookies.txt | cut -f2-)
curl -sk https://localhost:8080/api/v1/status -H "Cookie: dagger_kubernetes_access=$ACCESS"

# 4. Logs free of fatal/panic
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  logs statefulset/dagger-kubernetes-test-dagger-kubernetes --tail=100
```
Kill the port-forward after verification.

### 6.7 Optional but recommended E2E GC proof
Temporarily shorten the retention on the live release to observe a real
scale-down + GC, then restore:

```bash
# Shorten idle + retention for a fast proof
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.config.fleet.replicaIdleTtl=1m \
  --set supervisor.config.fleet.versionRetention=2m \
  --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s

# Port-forward and login (as 6.6), then provision a real engine:
curl -sk https://localhost:8080/v1/engines \
  -H "Cookie: dagger_kubernetes_access=$ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"minimum_engine_version":"v0.21.0"}'   # any version in the allowlist (0.19/0.20/0.21)

# Watch: engine pod appears, then scales down (~1m), then the STS is deleted (~2m).
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  get sts,svc,pvc,pods -l app=dagger-engine -w

# Confirm the version's StatefulSet AND its PVC are gone (PVC removed by
# WhenDeleted:Delete retention policy on the STS).
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  get sts,pvc -l version=v0.21.0   # expect no resources

# Restore defaults.
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
```
Verify the restored ConfigMap has `version_retention: "24h"`:
```bash
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  get cm dagger-kubernetes-test-dagger-kubernetes-config -o yaml | grep -A1 version_retention
```

### 6.8 Human UI verification (MANDATORY — agent cannot do it)
Request a human to confirm at `https://dagger.home.webcenter.fr`: login works;
the Runner Fleet view renders real cluster data; after the E2E GC proof the
provisioned version disappears from the Runners view (or is absent when idle).

## 7. Risks & rollback

- **Race (sweep delete vs concurrent Acquire)**: see §1. Accepted; 24h retention
  + idempotent re-provisioning bound the blast radius. A client that hits the
  window retries and re-creates the fleet.
- **Stale pinned sessions**: GC is skipped while any session pins the version
  (even though replicas are 0). The session store's `ReapOrphans` (lease TTL)
  eventually clears them, after which the idle clock restarts.
- **Annotation-only state**: idle-since is lost if an operator deletes the STS
  out-of-band (not via GC) — harmless (a re-created STS simply restarts the
  clock). No Raft table means no leader-gating of writes; the sweep runs on the
  supervisor that is the Raft leader only via normal operation, but even a
  non-leader pod's sweep writes only the STS annotation (not FSM), so no
  split-brain risk.
- **Ordering of deletion** (Service then STS) prevents orphaned headless
  Services; documented in §1.
- **Rollback**: revert the Helm value to `versionRetention: "0"` (or `"0s"`),
  which disables GC without any code rollback; annotations already written are
  inert (never read when disabled). Full rollback = `git revert` the changeset
  and redeploy via §6.2–6.6. No schema/data migration is involved (annotation
  on STS is disposable).

## 8. Out of scope
- No new Prometheus metric for fleet GC events (can be added later in
  `observ.Metrics`).
- No `min_replicas_per_version` warm pool (separate historical feature).
- No Raft FSM-backed idle tracking.
