# syntax=docker/dockerfile:1

# APP_VERSION is baked into the UI footer (NUXT_PUBLIC_APP_VERSION). The
# release workflow passes the release tag; other builds fall back to "unknown".
ARG APP_VERSION=unknown

# ---------- UI builder ----------
FROM node:22-alpine AS ui-builder
ARG APP_VERSION
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci
COPY ui/ ./
ENV NUXT_PUBLIC_APP_VERSION=$APP_VERSION
RUN npm run build

# ---------- Go builder ----------
FROM golang:1.27-alpine AS go-builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=ui-builder /ui/.output/public ./internal/handler/ui-dist/
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/supervisor ./cmd/api/

# ---------- dagger-kubernetes-ci builder ----------
FROM go-builder AS ci-builder
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/dagger-kubernetes-ci ./cmd/ci/

# ---------- Runner (Cloud Checks / workflow Jobs) ----------
# Separate stage so the supervisor image stays the final/default target. The
# runner needs git (clone) and the CI wrapper; the dagger CLI is provisioned
# on demand from the supervisor unless DAGGER_CLI_VERSION is set to pre-bake it.
FROM alpine:3.21 AS runner
ARG DAGGER_CLI_VERSION=""
RUN apk add --no-cache ca-certificates tzdata git \
    && update-ca-certificates \
    && addgroup -S dagger && adduser -S -G dagger -u 10001 dagger
RUN if [ -n "$DAGGER_CLI_VERSION" ]; then \
      apk add --no-cache curl; \
      curl -fsSL https://dl.dagger.io/dagger/install.sh | DAGGER_VERSION="$DAGGER_CLI_VERSION" sh; \
      apk del curl; \
    fi
COPY --from=ci-builder /out/dagger-kubernetes-ci /usr/local/bin/dagger-kubernetes-ci
USER dagger
ENTRYPOINT ["/usr/local/bin/dagger-kubernetes-ci"]

# ---------- Runtime ----------
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata git \
    && update-ca-certificates \
    && addgroup -S dagger && adduser -S -G dagger -u 10001 dagger
COPY --from=go-builder /out/supervisor        /usr/local/bin/supervisor
COPY --from=ci-builder /out/dagger-kubernetes-ci   /usr/local/bin/dagger-kubernetes-ci
COPY config/config.app.yaml.sample             /etc/dagger-kubernetes/config.app.yaml

USER dagger
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/supervisor"]
CMD ["--config=/etc/dagger-kubernetes/config.app.yaml"]
