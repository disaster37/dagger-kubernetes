# syntax=docker/dockerfile:1

# ---------- UI builder ----------
FROM node:22-alpine AS ui-builder
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci || npm install
COPY ui/ ./
RUN npm run build

# ---------- Go builder ----------
FROM golang:1.26-alpine AS go-builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=ui-builder /ui/dist ./internal/api/ui-dist/
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/supervisor ./cmd/supervisor/

# ---------- dagger-cache-ci builder ----------
FROM go-builder AS ci-builder
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/dagger-cache-ci ./cmd/dagger-cache-ci/

# ---------- Runtime ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && update-ca-certificates \
    && addgroup -S dagger && adduser -S -G dagger -u 10001 dagger
COPY --from=go-builder /out/supervisor        /usr/local/bin/supervisor
COPY --from=ci-builder /out/dagger-cache-ci   /usr/local/bin/dagger-cache-ci
COPY config.app.yaml.sample                   /etc/dagger-cache/config.app.yaml.sample

USER dagger
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/supervisor"]
CMD ["--config=/etc/dagger-cache/config.app.yaml"]
