# ADR-044: Jenkins shared library as a release artifact

- **Status:** accepted
- **Date:** 2026-09-25
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

1. **Recommended — dedicated minimal repository:** host only
   `ci-integrations/jenkins/` in a dedicated repository (or branch), seeded
   from the release artifact, and keep the `modernSCM` git retriever pointing
   at it. Cloning drops from the full repository to a handful of Groovy files.
   *(Creating that repository is a repo-owner follow-up, out of scope here.)*
2. **Fallback — filesystem extraction:** download and extract the tar.gz into
   a library directory on the Jenkins controller (init/entrypoint step) and
   register that directory as the library location instead of a git retriever.

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
  repository; releases carry a small, reproducible, checksumable asset;
  packaging is validated by the standard `ci` gate on every run.
- **Negative / risks:** the dedicated-minimal-repo path needs a repo-owner
  action before operators can drop the full-repo clone; the filesystem
  fallback moves update delivery from git to an operator-managed extraction
  step. The version string is validated rather than inferred from git, so
  callers must pass the release tag explicitly (the release workflow does).
