# dagger

## Run ci on local

It will run the following steps:
  - Call `ci` function


```bash
# Default local execution
dagger call -m 'github.com/disaster37/dagger-library-go/helm@v2' --src '.' 'ci' --ci github --version env:VERSION --registry-username env:REGISTRY_USERNAME --registry-password env:REGISTRY_PASSWORD --git-token env:GIT_TOKEN --git-repo-url env:GIT_REPO_URL --git-branch env:BRANCH export --path .
```
