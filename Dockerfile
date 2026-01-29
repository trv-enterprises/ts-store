# Copyright (c) 2026 TRV Enterprises LLC
# Licensed under the PolyForm Noncommercial License 1.0.0

FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install git for go mod download (some dependencies may need it)
RUN apk add --no-cache git

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o tsstore ./cmd/tsstore

# Final stage - minimal image
FROM alpine:3.19

# Add ca-certificates for HTTPS and tzdata for timezone support
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN adduser -D -u 1000 tsstore

# Copy binary from builder
COPY --from=builder /app/tsstore /usr/local/bin/tsstore

# Create data directory
RUN mkdir -p /data && chown tsstore:tsstore /data

# Switch to non-root user
USER tsstore

# Set working directory
WORKDIR /data

# Default environment
ENV TSSTORE_DATA_PATH=/data
ENV TSSTORE_HOST=0.0.0.0
ENV TSSTORE_PORT=8080
ENV TSSTORE_MODE=release

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Default command
CMD ["tsstore", "serve"]
