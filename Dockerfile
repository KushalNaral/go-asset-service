FROM golang:1.25-alpine AS builder

RUN apk add --no-cache \
    build-base \
    vips-dev \
    pkgconf

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# You can keep CGO_ENABLED=1 here if needed
RUN CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /asset-service \
    .

# ───────────────────────────────────────────────────────────────
#  Runtime stage
# ───────────────────────────────────────────────────────────────
FROM alpine:3.23

RUN apk add --no-cache \
    ca-certificates \
    vips \
    tzdata

# Non-root user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /asset-service .
# /data holds stats.json (STATS_FILE) so hit counts survive restarts
RUN mkdir -p /data && chown -R appuser:appgroup /app /data

USER appuser

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD wget -q --spider http://127.0.0.1:8080/health || exit 1

CMD ["/app/asset-service"]
