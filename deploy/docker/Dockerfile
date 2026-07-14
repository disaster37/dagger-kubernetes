FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /supervisor ./cmd/supervisor/

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /supervisor /usr/local/bin/supervisor
COPY config.app.yaml /etc/dagger-cache/config.app.yaml

EXPOSE 8080 8443

ENTRYPOINT ["/usr/local/bin/supervisor"]
CMD ["--config=/etc/dagger-cache/config.app.yaml"]
