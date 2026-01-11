# Build stage
FROM golang:1.23-bookworm AS builder

# Install libvips + build tools in one layer
RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
        libvips-dev \
        pkg-config \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /asset-service ./main.go

# Final runtime image (small)
FROM debian:bookworm-slim

# Install only runtime libvips (much smaller)
RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
        libvips42 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

# Run as non-root user (good practice)
RUN useradd -m appuser
USER appuser

WORKDIR /app

# Copy only the compiled binary
COPY --from=builder /asset-service /app/asset-service

# Where your images live (you'll mount this volume)
VOLUME ["/storage"]

EXPOSE 8080

CMD ["/app/asset-service"]
