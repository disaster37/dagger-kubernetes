# ADR-044: Jenkins shared library as a release artifact

- **Status:** accepted
- **Date:** 2026-09-25
- **Updated:** 2026-09-26 (dedicated repo + submodule)
- **Deciders:** dagger-kubernetes maintainers
- **Issue:** https://github.com/disaster37/dagger-kubernetes/issues/29

## Context

Jenkins consumes the `daggerKubernetes` shared library by registering a global
pipeline library in Jenkins-as-Code (JCasC) with a `modernSCM` git retriever
pointing at the **whole** `disaster37/dagger-kubernetes` repository with
`libraryPath: "ci-integrations/jenkins"`. Every pipeline therefore clones the
entire repository (Go sources, Helm chart, UI, tests) onto the Jenkins
controller — a large, repeated disk and network cost for a directory that
currently holds exactly one Groovy file
(`vars/daggerKubernetes.groovy`).

The issue frames this as a defect (resource waste), not new platform
capability. Constraints discovered while designing the fix (verified against
the official Jenkins documentation):

- Jenkins global shared libraries support **only SCM retrieval**
  (`modernSCM` / `legacySCM` with an SCM block). There is **no native
  HTTP / tar.gz / URL retriever** for a global library, and JCasC's
  `globalLibraries[].retriever` only models SCM blocks.
- The Groovy sources must stay in this repository as the single source of
  truth; `ci-integrations/gha/` and `ci-integrations/drone/` are distributed
  differently and are out of scope.

## Decision

Ship the Jenkins shared library as a **small, versioned, deterministic
`tar.gz` release asset** built by the local Dagger module.

### 1. `jenkins-libs` Dagger function

`JenkinsLibs(ctx, version)` in `dagger/main.go` packages only
`ci-integrations/jenkins/` (validated to be non-empty) into
`jenkins-libs-<version>.tar.gz`:

- `artifact.Filename(version)` validates the version (non-empty, ≤128 chars,
  `[A-Za-z0-9][A-Za-z0-9._-]*`, no `..`) before it reaches a path or the shell
  command line (CWE-78 / CWE-22), then derives the artifact filename.
- Packaging runs GNU `tar | gzip -n` in the pinned `golang:1.26` image with
  `--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner`: stable entry
  order, fixed epoch mtime, fixed ownership, no gzip header timestamp/name.
  Identical input therefore yields a **byte-identical archive**
  (checksumable releases).
- The archive is rooted at the library root (`./vars/`, `./src/`,
  `./resources/`), the standard Jenkins shared-library layout.
- `Ci` calls `JenkinsLibs(ctx, "dev")` on every run and returns
  `out/jenkins-libs-dev.tar.gz`, proving packaging works on each PR/push
  (only `out/bin/` and `out/coverage.out` are uploaded by `ci.yml`).

### 2. Publication on release

`release.yml` gains a `publish-jenkins-libs` job (trigger: `release: created`)
that installs the pinned Dagger CLI (`0.21.8`, matching `ci.yml` and
`dagger.json`'s `engineVersion`), runs
`jenkins-libs --version ${{ github.ref_name }} export`, and uploads
`jenkins-libs-<tag>.tar.gz` to the release with `gh release upload`
(`permissions: contents: write`).

### 3. Consumption paths (because JCasC is SCM-only)

The artifact cannot be a JCasC retriever, so two paths are documented in
`docs/README.md`:

1. **Primary — dedicated repository (JCasC-native):** the dedicated repository
   [`disaster37/dagger-kubernetes-jenkins`](https://github.com/disaster37/dagger-kubernetes-jenkins)
   now exists and is tracked in this repository as a git submodule at
   `ci-integrations/jenkins` (pinned to a detached SHA; `.gitmodules` records
   `branch = main`). The repository root *is* the library root, so JCasC
   consumes it via the `modernSCM` git retriever with **no `libraryPath`** and
   cloning drops from the whole repository to a single Groovy file
   (see §4).
2. **Fallback — filesystem extraction:** download and extract the tar.gz into
   a library directory on the Jenkins controller (init/entrypoint step) and
   register that directory as the library location instead of a git retriever.

### 4. Dedicated repository + submodule (resolved follow-up)

The follow-up that §3 deferred is now done (issue #29, increment 2):

- **Seeding:** `disaster37/dagger-kubernetes-jenkins` was created and seeded
  with `vars/daggerKubernetes.groovy` copied byte-identically from this
  repository's `ci-integrations/jenkins`, plus a `README.md` and this
  repository's `LICENSE` (verbatim, MIT — same copyright holder and year, so
  attribution and license terms are unchanged). The dedicated repo's root is
  the shared-library root (`vars/` at the top level).
- **Pinning policy:** the main repository tracks the dedicated repo as a git
  submodule at `ci-integrations/jenkins`, pinned to a **detached SHA** (the
  gitlink is what CI checks out, so builds stay reproducible), with
  `.gitmodules` recording the absolute-https `url` and `branch = main` so the
  documented bump procedure (`git submodule update --remote`) works. Updates
  flow through a **2-PR workflow**: PR + merge in the dedicated repo, then a
  second PR here that bumps the pin. Both `ci.yml` and the
  `publish-jenkins-libs` checkout in `release.yml` gained
  `submodules: true` so `actions/checkout` materializes the pinned working
  tree.
- **Determinism exclusion:** the submodule working tree contains a `.git`
  pointer file (checkout-specific path). `JenkinsLibs` selects its source with
  `.WithoutFile(".git")` before the empty-check and the mount, so `.git` never
  reaches the tar and the archive is byte-identical across clones and repeats.
  Empirically (engine v0.21.8, two consecutive builds — identical sha256),
  `tar tzf` lists exactly `./`, `./LICENSE`, `./README.md`, `./vars/`,
  `./vars/daggerKubernetes.groovy` (no `./.git`) and the sha256 for
  `jenkins-libs --version v0.1.0` is
  `e699a216256364d1afaacb851aaaff1c48c96925ec13feda3415fd847a48e4b0` —
  the pre-seed part-1 hash changes because the seeded repo root adds
  `README.md` and `LICENSE` next to `vars/`.

## Alternatives rejected

- **Keep the full-repo clone (status quo):** simplest, but every pipeline
  clones the whole repository onto the controller — the waste reported in
  issue #29.
- **Build the tar.gz with ad-hoc shell** (outside the Dagger module): no CI
  validation, drifts from the pinned tool images, and would bypass the
  `ci` gate that proves packaging on every PR.
- **Native tar.gz/URL retriever in JCasC:** unavailable — Jenkins global
  libraries are SCM-only (verified against the official docs). Documented as
  the hard constraint rather than worked around.

## Consequences

- **Positive:** Jenkins clones a handful of Groovy files instead of the whole
  repository (the dedicated repo `disaster37/dagger-kubernetes-jenkins` is
  registered directly, no `libraryPath`); releases carry a small, reproducible,
  checksumable asset; packaging is validated by the standard `ci` gate on every
  run.
- **Negative / risks:** the submodule must be initialized
  (`git submodule update --init`, or clone `--recurse-submodules`) or the `ci`
  gate fails with the hinted empty-directory error; the filesystem fallback
  moves update delivery from git to an operator-managed extraction step. The
  version string is validated rather than inferred from git, so callers must
  pass the release tag explicitly (the release workflow does).
