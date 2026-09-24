# Build stage
FROM golang:1.23-alpine AS builder

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

# Install git and ca-certificates for runtime
RUN apk add --no-cache git ca-certificates

# Create non-root user
RUN adduser -D -u 1000 gitsafe

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/gitsafe .

# Create data directory for persistent disk
RUN mkdir -p /var/data && chown gitsafe:gitsafe /var/data

# Switch to non-root user
USER gitsafe

# Expose port (Render sets PORT env var)
EXPOSE 8080

# Health check endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:${PORT:-8080}/healthz || exit 1

# Run the server
ENTRYPOINT ["./gitsafe"]