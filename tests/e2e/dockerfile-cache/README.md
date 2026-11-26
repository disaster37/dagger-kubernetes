# Dockerfile layer-cache stamp fixture (ADR-061 V4)

Proves the engine behaviour shipped in Dagger v0.21.8: a Dockerfile `COPY`
layer survives changes to files outside the copied subtree (the whole build
context used to be part of the layer key), while a change inside `app/` still
rebuilds it.

## Layout

| Path | Purpose |
|---|---|
| `Dockerfile` | `COPY app /app` plus two `RUN` steps that write random stamps |
| `app/main.txt` | the copied subtree's only file |
| `unrelated.txt` | a context file outside the `COPY` subtree |

`/copy-stamp` is regenerated whenever the `COPY` layer rebuilds;
`/mount-stamp` whenever the bind-mounted file changes.

## Run

Use a **fresh CLI process per build** (the host directory is session-cached):

```bash
tmp=$(mktemp -d)
cp -R tests/e2e/dockerfile-cache/Dockerfile \
      tests/e2e/dockerfile-cache/app \
      tests/e2e/dockerfile-cache/unrelated.txt "$tmp/"
mkdir -p "$tmp/.git"
echo ref > "$tmp/.git/HEAD"

stamps() {
  dagger core container from --address "$tmp" \
    with-exec --args sh --args -c --args 'cat /copy-stamp /mount-stamp' \
    stdout
}

a=$(stamps)
echo "changed unrelated.txt" >> "$tmp/unrelated.txt"
echo changed >> "$tmp/.git/HEAD"
b=$(stamps)                 # a == b: the COPY layer was reused
echo "changed app" >> "$tmp/app/main.txt"
c=$(stamps)                 # c != a: the COPY layer was rebuilt
```

Expected: `a == b`, `c != a`. This is a report-only proof (never a CI gate).
Any Dagger engine ≥ v0.21.8 exhibits it; the same commands on v0.21.7 make
`a != b` because the whole context keyed the layer.
