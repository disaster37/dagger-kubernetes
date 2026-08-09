# Golang module

Dagger module for Go project lifecycle: build, test, lint, format, vulnerability scanning, and CI pipeline generation.

## Quick start

```bash
dagger call -m github.com/disaster37/dagger-library-go/golang@2.0.10 --src . ci export --path .
```

## Functions

### `New` — Initialize

| Arg | Required | Default | Description |
|-----|----------|---------|-------------|
| `base` | no | auto (from go.mod) | Custom Go container |
| `version` | no | auto | Go version when no go.mod |
| `src` | yes | — | Source directory |

### `Ci` — Full CI pipeline

Runs lint, test, and build as a single CI entrypoint.

| Arg | Default | Description |
|-----|---------|-------------|
| `main` | — | Path to main.go |
| `out` | — | Binary output name |
| `os` | — | Target OS |
| `arch` | — | Target arch |
| `ldflags` | `["-s", "-w"]` | Linker flags |

### `GenerateCi` — Generate CI pipeline files

Generates a CI pipeline file for GitHub Actions, Jenkins, or GitLab CI.

| Arg | Default | Description |
|-----|---------|-------------|
| `ci` | (required) | `github`, `jenkins`, or `gitlab` |
| `branches` | `["main"]` | Branches that trigger the pipeline |
| `daggerVersion` | current | Dagger CLI version |
| `defaultBranch` | `main` | Branch used for tag commits |
| `moduleRef` | auto (current version) | Configurable module reference |
| `registryUsernameKey` | — | GitHub secret name for username |
| `registryPasswordKey` | — | GitHub secret name for password |
| `registryCredential` | — | Jenkins credential ID for registry |
| `gitTokenCredential` | — | Jenkins credential ID for git token |
| `registryUsernameVar` | — | GitLab CI/CD variable name for registry username |
| `registryPasswordVar` | — | GitLab CI/CD variable name for registry password |
| `gitTokenVar` | — | GitLab CI/CD variable name for git token |

```bash
# Generate GitHub Actions workflow
dagger call -m dagger/golang --src . generate-ci --ci github export --path .

# Generate Jenkins pipeline
dagger call -m dagger/golang --src . generate-ci --ci jenkins --registry-credential my-creds --git-token-credential my-token export --path .

# Generate GitLab CI pipeline
dagger call -m dagger/golang --src . generate-ci --ci gitlab --registry-username-var REGISTRY_USER --registry-password-var REGISTRY_PASS --git-token-var GIT_TOKEN export --path .
```
