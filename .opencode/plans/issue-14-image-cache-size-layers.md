# Plan: Fix "Image cache Size/Layers always 0" + nav reorder (issue #14)

- **Issue:** https://github.com/disaster37/dagger-kubernetes/issues/14
- **Branch:** `feat/s3-cache-backend` (HEAD `0e987141c79c2d8feb07802977e7e57996d3bda2`) — **do not create a new branch or PR**; this belongs to the current changeset.
- **Goal:** The admin `/image-cache` page must show nonzero Size/Layers for multi-arch (index) tags, and the top-nav `Image cache` link must sit immediately **before** `Runners` (still admin-only).

## Scope

**In scope**
1. Resolve OCI image indexes / Docker manifest lists to a representative child platform manifest so `size_bytes`/`layer_count` are nonzero for multi-arch tags.
2. UI: render `layer_count < 0` as `unknown`; add a short tooltip on Size/Layers headers.
3. Nav: move `Image cache` before `Runners`, admin-only.
4. Tests (unit + integration) proving the resolution, edge cases, and that prune still works.
5. Docs (ADR-034, `docs/README.md`) + mandatory live redeploy/validation + `AGENTS.local.md` revision entry.

**Out of scope (explicitly)**
- Prune semantics (deletion still targets the top-level/index digest — unchanged).
- S3/Zot backend changes, per-platform row UI, new config keys, Helm chart changes.
- Any `domain.DistributionClient` interface signature change (`ManifestSize(ctx, repo, tag) (digest, size, layers, err)` stays).

---

## 1. Root cause (verified)

Listing pipeline: `ui/src/imagecache/ImageCache.vue` → `ui/src/api/client.ts#fetchImageCacheInfo` → `GET /api/v1/image-cache` (`internal/handler/image_cache.go`) → `service.ImageCacheService.List` (`internal/service/image_cache.go:130-142`) → `repository.DistributionClient.ManifestSize` (`internal/repository/registry_client.go:315-336`).

`ManifestSize` (`registry_client.go:315-336`) decodes the fetched manifest and sums `config` + `layers` descriptor sizes. Zot mirrors of Docker Hub tags (e.g. `library/alpine:3.20`) resolve to **OCI image indexes / Docker manifest lists** (`application/vnd.oci.image.index.v1+json` / `application/vnd.docker.distribution.manifest.list.v2+json`). Those have **no top-level `config`/`layers`** — only `manifests[]` child descriptors — so the sum is `size=0, layers=0`. The `-1` "unknown" sentinel is only emitted when `size==0 && len(layers)>0` (`registry_client.go:331`), which never fires for an index (zero layers).

The returned `digest` is **also** used by prune (`service.pruneRef`, `internal/service/image_cache.go:329-345`): tag → `ManifestSize` digest → `DELETE /v2/<repo>/manifests/<digest>`. **The returned digest must remain the top-level (index) digest** — deleting a child platform manifest would not unlink the tag.

This is documented in `AGENTS.local.md` §7 revision-54 ("`size_bytes`/`layer_count` are 0 for these tags because they resolve to OCI image indexes and the client only sums a direct manifest's config+layers — pre-existing client behavior").

---

## 2. Ordered implementation steps

### Step 1 — `internal/repository/registry_client.go`

#### 1a. Extend the decode structs (replace lines 245-255)

```go
// manifest is the subset of the OCI/Docker manifest needed to sum sizes and to
// resolve an image index to a representative child platform manifest.
type manifest struct {
	MediaType   string            `json:"mediaType"`
	Config      *descriptor       `json:"config"`
	Layers      []descriptor      `json:"layers"`
	Manifests   []descriptor      `json:"manifests"`
	Annotations map[string]string `json:"annotations"`
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    platform          `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}
```

#### 1b. Add index-resolution constants/helpers (after the `manifestAccept` const, ~line 257)

```go
const manifestAccept = "..." // unchanged, keep as-is

// OCI image index / Docker manifest list media types, and the BuildKit
// attestation annotation used to skip SBOM/provenance child descriptors.
const (
	ociIndexMediaType           = "application/vnd.oci.image.index.v1+json"
	dockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	annotationReferenceType     = "vnd.docker.reference.type"
	referenceTypeAttestation    = "attestation-manifest"

	// maxIndexDepth bounds how many nested index levels ManifestSize resolves
	// before reporting unknown. Docker Hub tags are a single index level;
	// the cap guards against pathological index-in-index chains.
	maxIndexDepth = 2
)

// isIndexManifest reports whether m is an image index / manifest list. It
// matches the two index media types and falls back to the structural signal
// (no config but children present) for registries that omit mediaType.
func isIndexManifest(m *manifest) bool {
	switch m.MediaType {
	case ociIndexMediaType, dockerManifestListMediaType:
		return true
	}
	return m.Config == nil && len(m.Manifests) > 0
}

// isAttestation reports whether a child descriptor points at an attestation
// manifest (BuildKit SBOM/provenance), which has no runnable config/layers and
// must not be chosen as the representative child.
func isAttestation(d descriptor) bool {
	return d.Annotations[annotationReferenceType] == referenceTypeAttestation
}

// selectChildManifest picks a representative child descriptor to size. It
// prefers linux/amd64, skips attestation and "unknown"-platform entries, and
// falls back to the first usable non-attestation descriptor. ok is false when
// no usable child exists (empty index, or only attestation/unknown entries).
func selectChildManifest(m *manifest) (descriptor, bool) {
	var fallback descriptor
	haveFallback := false
	for _, d := range m.Manifests {
		if isAttestation(d) {
			continue
		}
		if d.Platform.Architecture == "unknown" {
			continue
		}
		if !haveFallback {
			fallback = d
			haveFallback = true
		}
		if d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
			return d, true
		}
	}
	if haveFallback {
		return fallback, true
	}
	return descriptor{}, false
}

// directManifestSize sums a non-index manifest's config+layers. The -1
// "unknown" sentinel is set when the manifest has layers but their sizes are
// absent (the registry omitted descriptor sizes) — the existing behavior.
func directManifestSize(m *manifest) (size, layers int64) {
	layers = int64(len(m.Layers))
	for _, l := range m.Layers {
		size += l.Size
	}
	if m.Config != nil {
		size += m.Config.Size
	}
	if size == 0 && len(m.Layers) > 0 {
		size = -1
	}
	return size, layers
}
```

#### 1c. Refactor `getManifest` into a shared `getManifestRef` (replace lines 259-309)

Keep `getManifest` as the tag wrapper; add `getManifestByDigest`; move the body into `getManifestRef` (behavior unchanged for the tag path).

```go
// getManifest fetches repo:tag's manifest, mapping 404 to ErrManifestNotFound
// and other non-2xx to ErrRegistryUnreachable. It returns the decoded manifest
// plus its digest (from Docker-Content-Digest, or computed from the body).
func (c *DistributionClient) getManifest(ctx context.Context, repo, tag string) (*manifest, string, error) {
	return c.getManifestRef(ctx, repo, tag, false)
}

// getManifestByDigest fetches repo's manifest by sha256 digest (used to resolve
// an image index to a representative child). The digest is validated before it
// is interpolated into the request path.
func (c *DistributionClient) getManifestByDigest(ctx context.Context, repo, digest string) (*manifest, string, error) {
	return c.getManifestRef(ctx, repo, digest, true)
}

// getManifestRef is the shared manifest GET. When refIsDigest is true, ref is
// validated with validDigest and path-escaped; otherwise it is escaped as a
// tag. Everything else (Accept header, status mapping, digest header
// validation with body-hash fallback) is common to both.
func (c *DistributionClient) getManifestRef(ctx context.Context, repo, ref string, refIsDigest bool) (*manifest, string, error) {
	repoPath, err := escapeRepository(repo)
	if err != nil {
		return nil, "", err
	}
	var refPath string
	if refIsDigest {
		if !validDigest(ref) {
			return nil, "", fmt.Errorf("invalid digest: must be sha256:<hex>")
		}
		refPath = url.PathEscape(ref)
	} else {
		refPath, err = escapeTag(ref)
		if err != nil {
			return nil, "", err
		}
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), repoPath, refPath), manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		discard(resp)
		return nil, "", fmt.Errorf("%w: %s:%s", ErrManifestNotFound, repo, ref)
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp)
		return nil, "", fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	// Only trust a registry-supplied digest when it has the expected
	// sha256:<hex> shape; otherwise compute it from the body (CWE-20/CWE-918).
	if digest != "" && !validDigest(digest) {
		digest = ""
	}
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("decode manifest: %w", err)
	}
	return &m, digest, nil
}
```

#### 1d. Rewrite `ManifestSize` + add `manifestSize` resolver (replace lines 311-336)

```go
// ManifestSize fetches the manifest for repo:tag and returns (digest, sizeBytes,
// layerCount). digest is always the TOP-LEVEL manifest digest (the index digest
// for a multi-arch tag), which the prune path uses to unlink the tag. For an
// image index / manifest list, sizeBytes/layerCount are resolved from a
// representative child platform manifest (linux/amd64 preferred, attestation
// and "unknown"-platform entries skipped); when no child resolves (empty index,
// only attestation entries, child fetch/decode failure, or nesting past
// maxIndexDepth) both are -1 (unknown). A child-resolution failure is NOT an
// error — the tag stays in the listing. Returns ErrManifestNotFound on 404 of
// the top-level manifest.
func (c *DistributionClient) ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error) {
	m, digest, err := c.getManifest(ctx, repo, tag)
	if err != nil {
		return "", 0, 0, err
	}
	size, layers = c.manifestSize(ctx, repo, m, 0)
	return digest, size, layers, nil
}

// manifestSize resolves m's (size, layers). Index manifests are resolved to a
// representative child (descending through at most maxIndexDepth nested indexes,
// then -1/-1); direct manifests are summed in place.
func (c *DistributionClient) manifestSize(ctx context.Context, repo string, m *manifest, depth int) (int64, int64) {
	if !isIndexManifest(m) {
		return directManifestSize(m)
	}
	if depth >= maxIndexDepth {
		return -1, -1
	}
	child, ok := selectChildManifest(m)
	if !ok {
		return -1, -1
	}
	cm, _, err := c.getManifestByDigest(ctx, repo, child.Digest)
	if err != nil {
		return -1, -1
	}
	return c.manifestSize(ctx, repo, cm, depth+1)
}
```

`DeleteManifest` is unchanged. No change to `escapeRepository`/`escapeTag`/`do`/`readBounded`.

#### 1e. Update the `domain.DistributionClient` comment (`internal/domain/image_cache.go:49-51`)

Replace the `ManifestSize` doc line with:

```go
	// ManifestSize returns (digest, sizeBytes, layerCount). digest is the
	// top-level manifest digest (the index digest for multi-arch tags) and is
	// used by the prune path for tag→digest resolution; size/layerCount are
	// used by listing. For image indexes / manifest lists, size/layerCount are
	// resolved from a representative child platform manifest (linux/amd64
	// preferred); -1 = unknown when no child resolves.
	ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error)
```

This is a comment-only change; the interface signature is untouched.

**Edge-case table (all must hold):**

| Case | Behavior |
|---|---|
| Direct (non-index) manifest | unchanged: `config.size + Σlayers.size`, `layers=len(layers)`, `size==0 && len(layers)>0 → -1` |
| Index, has `linux/amd64` child | child config+layers summed, `digest` = index digest |
| Index, no amd64 (e.g. only arm64) | fallback first non-attestation, non-`unknown` child |
| Index with attestation + `unknown`-arch children | skipped during selection |
| Index with zero children | `(-1, -1)`, `digest` = index digest, `err=nil` |
| All children attestation/unknown | `(-1, -1)`, `err=nil` |
| Child fetch 404 / transport / decode / invalid digest | `(-1, -1)`, `err=nil` (tag NOT dropped) |
| Nested index child | recurse up to `maxIndexDepth=2` levels, then `(-1,-1)` |
| Prune by tag | digest = index digest → `DELETE .../manifests/<index-digest>` unlinks the tag |

---

### Step 2 — `internal/repository/registry_client_test.go`

Add a new **table-driven** test `TestRegistryManifestSizeIndex` (index cases need multi-request routing, so keep them separate from the single-response cases in `TestRegistryManifestSize`). Use `testClient` (already exists) and `digestRepeat` for realistic digests. The handler routes on `r.URL.Path` (tag vs child digest).

```go
// childDigest is a valid sha256:<64 hex> digest for a child manifest fixture.
func childDigest(c string) string { return digestRepeat(c) }

func TestRegistryManifestSizeIndex(t *testing.T) {
	const (
		amd64   = "sha256:" + strings.Repeat("a", 64)
		arm64   = "sha256:" + strings.Repeat("b", 64)
		attest  = "sha256:" + strings.Repeat("c", 64)
		nested  = "sha256:" + strings.Repeat("d", 64)
		indexDig = "sha256:" + strings.Repeat("e", 64)
	)

	// indexBody renders an OCI image index with the given manifests array JSON.
	// amd64Body/arm64Body render direct manifests whose config+layers sum to
	// the given total with the given layer count (config = each layer = total/(layers+1)).
	...

	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantDigest string
		wantSize   int64
		wantLayers int64
	}{
		// 1. index → linux/amd64 child wins; digest is the INDEX digest
		// 2. index → no amd64, arm64 fallback
		// 3. index → attestation + unknown-platform entries skipped (amd64 still wins)
		// 4. index → child 404 → (-1,-1), nil error, digest = index
		// 5. empty index (manifests: []) → (-1,-1)
		// 6. all-attestation index → (-1,-1)
		// 7. nested index (child is itself an index) → resolves grandchild
		// 8. nested index past depth cap → (-1,-1)
	}
	for _, tc := range tests { ... c.ManifestSize(ctx, "dagger-cache", "v0-21-4") ... }
}
```

Representative handler for case 1 (others follow the same routing shape):

```go
func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/dagger-cache/manifests/v0-21-4":
		w.Header().Set("Content-Type", ociIndexMediaType)
		w.Header().Set("Docker-Content-Digest", indexDig)
		_, _ = w.Write([]byte(`{"mediaType":"` + ociIndexMediaType + `","manifests":[
			{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + amd64 + `","size":300,"platform":{"os":"linux","architecture":"amd64"}},
			{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + arm64 + `","size":310,"platform":{"os":"linux","architecture":"arm64"}},
			{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + attest + `","size":50,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}
		]}`))
	case "/v2/dagger-cache/manifests/" + amd64:
		w.Header().Set("Docker-Content-Digest", amd64)
		_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":100},"layers":[{"digest":"sha256:l1","size":100},{"digest":"sha256:l2","size":100}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
```

> Note: the `testClient` helper uses `ts.Listener.Addr().String()`; the Go httptest server will present `r.URL.Path` with the digest's `:` intact (colon is not path-escaped by `url.PathEscape`). Match on `"/v2/dagger-cache/manifests/"+amd64`.

Add a hostile-input test:

```go
func TestManifestSizeIndexRejectsMalformedChildDigest(t *testing.T) {
	// An index whose only child carries a non-sha256 digest must NOT issue a
	// request with that digest; the client returns (-1,-1) unknown (CWE-20/CWE-918).
	var requested bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requested = true
		if strings.HasPrefix(r.URL.Path, "/v2/dagger-cache/manifests/") {
			// only the top-level tag fetch may occur
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			t.Errorf("unexpected manifest fetch path %q", r.URL.Path)
		}
		w.Header().Set("Docker-Content-Digest", digestRepeat("e"))
		_, _ = w.Write([]byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"../../v2/_catalog","platform":{"os":"linux","architecture":"amd64"}}]}`))
	})
	digest, size, layers, err := c.ManifestSize(context.Background(), "dagger-cache", "v0-21-4")
	if err != nil {
		t.Fatalf("ManifestSize: %v", err)
	}
	if size != -1 || layers != -1 {
		t.Fatalf("size/layers = %d/%d, want -1/-1", size, layers)
	}
	if !validDigest(digest) {
		t.Fatalf("digest = %q, want valid top-level digest", digest)
	}
	_ = requested
}
```

---

### Step 3 — `tests/integration/image_cache_test.go`

Extend the fake registry to serve index → child manifests, and add a black-box integration test proving nonzero size/layers end-to-end (API → service → client). No listener changes — it already uses `freeListener`/`httptest` (no-hardcoded-ports rule holds).

**3a.** Extend the fixtures:

```go
type fakeOCIImage struct {
	digest string
	size   int64
	layers int64
	// children, when non-nil, turns this tag into an OCI image index whose
	// entries are platform manifests served by digest.
	children []fakeOCIChild
}

type fakeOCIChild struct {
	digest      string
	os          string
	arch        string
	size        int64
	layers      int64
	attestation bool // BuildKit SBOM/provenance entry to be skipped
}

type fakeOCIRegistry struct {
	mu       sync.Mutex
	repos    map[string]map[string]fakeOCIImage
	children map[string]fakeOCIChild // digest -> child (flat manifest)
	srv      *httptest.Server
}
```

In `newFakeOCIRegistry`, after assigning `repos`, build `children` by scanning every `img.children` (key = `child.digest`).

**3b.** Refactor `serve`: extract the current flat-manifest body into a helper and route:

```go
// flatManifestBody renders a direct manifest whose config + layers sum to size
// with the given layer count (config and each layer = size/(layers+1), so the
// sum is exact for sizes divisible by layers+1).
func flatManifestBody(size, layers int64) []byte { ... }

// indexManifestBody renders an OCI image index for img.children.
func indexManifestBody(img fakeOCIImage) []byte { ... }
```

In the GET branch:

```go
case http.MethodGet, http.MethodHead:
	f.mu.Lock()
	img, ok := f.repos[repo][ref]
	var child *fakeOCIChild
	if !ok {
		if ch, ok2 := f.children[ref]; ok2 {
			child = &ch
			ok = true
		}
	}
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if child != nil {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", child.digest)
		_, _ = w.Write(flatManifestBody(child.size, child.layers))
		return
	}
	if img.children != nil {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", img.digest)
		_, _ = w.Write(indexManifestBody(img))
		return
	}
	// existing flat-manifest path (use flatManifestBody(img.size, img.layers))
```

`indexManifestBody` emits `mediaType` + `manifests[]` with per-child `digest`, `platform{os,architecture}` and (for `attestation`) `annotations:{"vnd.docker.reference.type":"attestation-manifest"}`.

**3c.** Add `TestImageCacheListIndexManifestIntegration` (reuse `newImageCacheTestEnv`):

```go
func TestImageCacheListIndexManifestIntegration(t *testing.T) {
	indexDig := fakeDigest("library/alpine", "3.20")
	fake := newFakeOCIRegistry(t, map[string]map[string]fakeOCIImage{
		"library/alpine": {
			"3.20": {digest: indexDig, children: []fakeOCIChild{
				{digest: "sha256:" + strings.Repeat("a", 64), os: "linux", arch: "amd64", size: 300, layers: 2},
				{digest: "sha256:" + strings.Repeat("b", 64), os: "linux", arch: "arm64", size: 310, layers: 2},
				{digest: "sha256:" + strings.Repeat("c", 64), os: "unknown", arch: "unknown", size: 50, layers: 1, attestation: true},
			}},
		},
	})
	env := newImageCacheTestEnv(t, fake)

	// List: the index tag must report the amd64 child's nonzero size/layers and
	// the INDEX digest.
	status, body := env.do(t, "GET", "/api/v1/image-cache", "")
	// assert status 200; mirror[0] reachable; repo library/alpine tag 3.20:
	//   digest == indexDig
	//   size_bytes == 300 (config 100 + 2×100)
	//   layer_count == 2
	//   (layer_count == 0 would be the bug)

	// Prune by tag still works: resolves to the index digest, unlinks the tag.
	status, body = env.do(t, "POST", "/api/v1/image-cache/prune",
		`{"mirror_id":"docker-io","refs":[{"repository":"library/alpine","tag":"3.20"}]}`)
	// assert 200, pruned == 1

	// Re-list: tag gone.
}
```

Use sizes divisible by `layers+1` (300/3=100, 310/3 would truncate — assert only `size_bytes > 0` + `layer_count == 2` for arm64, or pick divisible values) so exact assertions are deterministic.

---

### Step 4 — `ui/src/imagecache/ImageCache.vue`

1. Add a `formatLayers` helper next to `formatBytes` (line ~233):

```ts
function formatLayers(n: number): string {
  return n < 0 ? 'unknown' : String(n)
}
```

2. Change the Layers cell (line 76) `<td>{{ tag.layer_count }}</td>` → `<td>{{ formatLayers(tag.layer_count) }}</td>`.

3. Add `title` tooltips on the Size/Layers headers (lines 58-59):

```html
<th title="Multi-arch tags report a representative platform (linux/amd64 when present)">Size</th>
<th title="Multi-arch tags report a representative platform (linux/amd64 when present)">Layers</th>
```

`formatBytes` already returns `unknown` for `n < 0` (line 234) — no change needed for Size.

### Step 5 — `ui/src/App.vue` (nav order)

Move `Image cache` out of the admin `<template>` and place it **immediately before** `Runners`, keeping it admin-only with its own `v-if`:

```html
<router-link to="/pipelines">Pipelines</router-link>
<router-link to="/history">History</router-link>
<router-link v-if="auth.isAdmin" to="/image-cache">Image cache</router-link>
<router-link to="/fleet">Runners</router-link>
<router-link to="/services">Services</router-link>
<router-link to="/settings">Settings</router-link>
<router-link to="/connect">Connect</router-link>
<template v-if="auth.isAdmin">
  <router-link to="/admin/users">Users</router-link>
  <router-link to="/admin/groups">Groups</router-link>
  <router-link to="/admin/projects">Projects</router-link>
</template>
```

Final order: Pipelines, History, [Image cache if admin], Runners, Services, Settings, Connect, then admin Users/Groups/Projects. The `/image-cache` route already has `meta: { admin: true }` (`ui/src/router/index.ts:20`), so direct navigation stays guarded.

### Step 6 — Docs

- **`docs/design/ADR-034-admin-image-cache-management.md`** — add to **Consequences** (and reference from D5): "For multi-arch tags (OCI image index / Docker manifest list), `GET /api/v1/image-cache` reports `size_bytes`/`layer_count` from a representative child platform manifest (linux/amd64 preferred; attestation and `unknown`-platform entries skipped), while `digest` remains the top-level index digest so prune still unlinks the tag. If no child resolves, size/layer count are `-1` (unknown)."
- **`docs/README.md`** (image-cache section, ~line 791-792) — after "lists each mirror's repositories/tags with digest, size, and layer count", add: "For multi-arch tags the size/layer count reflects a representative platform (linux/amd64 when present); the digest is the top-level index digest."
- **`config/config.app.yaml.sample`** — **no change** (no config keys added/removed; verified `image_cache.mirrors` shape unchanged in `config/loader.go`).
- **`DAGGER.md`** — **no change** (`dagger/` and workflows untouched).

---

## 3. Test / verification plan (exact commands, in order)

Run from repo root unless noted:

```bash
gofmt -l .                     # expect NO output (no unformatted files)
go build ./...                 # passes
go vet ./...                   # passes
go test -race -covermode=atomic ./...   # passes, including new cases
cd ui && npm ci && npm run build && cd ..   # UI builds (Node 22)
rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist
git ls-files ui/dist internal/handler/ui-dist   # MUST list files (both are tracked; the embed requires them committed)
go build ./...                 # re-verify embed picks up the rebuilt SPA
```

**UI dist is tracked** (`.gitignore` only ignores the root `/ui-dist/`, not `ui/dist/` or `internal/handler/ui-dist/`). After any UI change, **both** `ui/dist/**` and `internal/handler/ui-dist/**` must be regenerated and committed in the same changeset (the Dockerfile rebuilds `ui/` → `internal/handler/ui-dist/`, but the committed copy is what local `go build` and CI's binary build embed via `//go:embed all:ui-dist` in `internal/handler/ui.go`).

**CI gate (mandatory, AGENTS.md):**

```bash
dagger call -m ./dagger --src . ci export --path out
```

If no Docker daemon is available, the minimum is:

```bash
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```

`golangci-lint` runs the `unused` linter — after the refactor, every new helper (`isIndexManifest`, `isAttestation`, `selectChildManifest`, `directManifestSize`, `manifestSize`, `getManifestByDigest`, `getManifestRef`, `platform`) must be referenced (they are, by `ManifestSize` and the test fixtures).

---

## 4. Live deploy + validation (mandatory, `AGENTS.local.md` §4–§6)

Do **not** copy secrets into any file — the admin password lives in `AGENTS.local.md` §9.

```bash
# 4.1 build (includes the UI via ui-builder stage)
docker build -t docker.io/disaster/dagger-kubernetes:dev .

# 4.2 push
docker push docker.io/disaster/dagger-kubernetes:dev

# 4.3 capture current values (IMPORTANT — never helm upgrade without -f)
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml

# 4.4 upgrade
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes \
  --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes

# 4.5 force rollout + wait (tag is mutable; must force re-pull)
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

**Agent verification (§5.1):**
1. Pods Ready: `kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test get pods -l app.kubernetes.io/name=dagger-kubernetes` → `Running`/`Ready`.
2. Probes (port-forward, `-k` for HTTPS): `/healthz` → 200 "ok"; `/readyz` → 200.
3. Authed `GET /api/v1/image-cache`: log in with the admin password (AGENTS.local.md §9) via `POST /api/v1/auth/login` `{username,password}` (sets the httpOnly session cookie; capture with `curl -c/-b`), then:
   `curl -sk https://localhost:8080/api/v1/image-cache -b cookies.txt`
   → for the docker-io mirror's index tags (e.g. `library/alpine:3.20`), **`size_bytes > 0` and `layer_count > 0`** (this is the fix); digest is the index `sha256:…`.
4. Logs clean: `kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test logs statefulset/dagger-kubernetes-test-dagger-kubernetes --tail=100` (no panic/fatal).
5. Nav: confirm `https://dagger.home.webcenter.fr/image-cache` shows the `Image cache` link immediately before `Runners`, and that it is absent for a non-admin user (admin-only preserved).

**Human verification (§5.2, mandatory):** after agent checks pass, request a human confirm on the live UI at `https://dagger.home.webcenter.fr`: Image cache page shows nonzero Size/Layers for multi-arch tags, nav order is correct, and prune still works. The implementer's final message MUST ask for this.

**Update `AGENTS.local.md` §3/§7** with a new revision entry (e.g. "revision 55") recording: image digest (`docker inspect --format '{{.RepoDigests}}' docker.io/disaster/dagger-kubernetes:dev`), what was validated (nonzero size/layers for index tags, nav order, prune regression), and that this fix is issue #14. Use the existing §7 entry style:

```
**2026-09-16 update (image-cache index size/layer resolution + nav reorder,
issue #14, revision 55, image digest `sha256:<digest>…`):** ManifestSize now
resolves OCI image indexes / manifest lists to a representative child
(linux/amd64 preferred) so size_bytes/layer_count are nonzero for multi-arch
tags; the returned digest stays the top-level index digest (prune unchanged).
The `Image cache` nav link moved immediately before `Runners` (still
admin-only). Validated: GET /api/v1/image-cache reports size_bytes>0 and
layer_count>0 for docker-io library/alpine:3.20 (digest sha256:d9e853e8…);
prune one re-lists without the tag; nav order confirmed on the live UI.
```

---

## 5. Acceptance criteria (checklist)

- [ ] `GET /api/v1/image-cache` returns `size_bytes > 0` and `layer_count > 0` for docker-io `library/alpine:3.20` (multi-arch) on the live cluster.
- [ ] Multi-arch tag `digest` in the API response is the top-level index digest (not a child platform digest).
- [ ] Prune by tag still unlinks the tag (prune → re-list drops it) — index digest used for delete.
- [ ] Child-resolution failure yields `size_bytes=-1, layer_count=-1` with the tag still listed (no tag dropped).
- [ ] `Image cache` nav link renders immediately before `Runners`, admin-only (absent for non-admin; route still `meta.admin`).
- [ ] UI renders `layer_count < 0` as `unknown`; Size/Layers headers carry the tooltip.
- [ ] `gofmt -l` clean; `go build ./...`, `go vet ./...`, `go test -race -covermode=atomic ./...` green.
- [ ] `cd ui && npm ci && npm run build` green; `ui/dist` + `internal/handler/ui-dist` regenerated and committed.
- [ ] CI gate `dagger call -m ./dagger --src . ci export --path out` green (or the documented no-daemon minimum).
- [ ] `golangci-lint` clean (no unused symbols from the refactor).
- [ ] Docs updated (ADR-034 Consequences, `docs/README.md`); `config.app.yaml.sample` and `DAGGER.md` unchanged (no config/CI changes).
- [ ] Live cluster redeployed + agent §5.1 checks green + `AGENTS.local.md` §3/§7 revision entry added.
- [ ] Human §5.2 verification requested (final implementer message).
- [ ] Branch still `feat/s3-cache-backend`; no new branch/PR.

---

## 6. Risks / open questions (with recommended resolution)

- **Representative platform vs aggregation.** The UI shows one number per tag; multi-arch tags report the `linux/amd64` child only. **Resolution (recommended):** representative platform is correct for a "how much space does this tag occupy" view; aggregation across platforms would inflate size and mislead prune. Documented in the ADR and the header tooltip.
- **Extra GET cost on prune.** `pruneRef` reuses `ManifestSize`, so pruning an index tag now issues one extra child GET. **Resolution:** accepted — in-cluster mirrors, bounded by the existing 10s per-request + 30s/10m operation budgets; no interface change (no digest-only fast path).
- **Depth cap.** Nested index chains are pathological; `maxIndexDepth=2` resolves the real one-level case and fails safe to `-1/-1` beyond. **Resolution:** accepted.
- **Size is "logical" (descriptor sum), not on-disk bytes.** Unchanged from today's contract; compressed blob bytes are not reported. **Resolution:** out of scope (pre-existing `ManifestSize` contract).

---

## 7. Rollback / idempotency

- **Rollback:** `helm --kubeconfig /home/user/.kube/home rollback dagger-kubernetes-test -n dagger-kubernetes-test` (to the previous revision, currently 54), or re-push the previous image digest and re-`--set supervisor.image.tag=<prev>` then rollout restart. Record the previous digest from `docker inspect ... RepoDigests` before upgrading.
- **Idempotency:** the fix is read-only on the registry (only GETs added); redeploying the same change is safe. Prune remains idempotent (absent manifest = already pruned). No storage/PVC/RBAC changes.
