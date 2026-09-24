# Build stage
FROM golang:1.24-alpine AS builder

# Install git and build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o gitsafe ./cmd/server

# Final stage
FROM alpine:3.20

# Install git, ca-certificates for runtime, and su-exec for privilege dropping
RUN apk add --no-cache git ca-certificates su-exec

# Create non-root user
RUN adduser -D -u 1000 gitsafe

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/gitsafe .

# Runtime entrypoint: makes the (possibly root-owned) persistent-disk mount
# writable to the gitsafe user, then drops privileges via su-exec.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Expose port (Render sets PORT env var)
EXPOSE 8080

# Health check endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:${PORT:-8080}/healthz || exit 1

# Starts as root so the entrypoint can chown the runtime mount, then execs the
# binary as the unprivileged gitsafe user (uid 1000) via su-exec.
ENTRYPOINT ["/entrypoint.sh"]
CMD ["./gitsafe"]