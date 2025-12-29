# Dockerfile for doltswarm integration tests
# Build: ./docker/build-docker.sh (from integration directory)
# Run:   docker run --rm -e PORT=10500 doltswarmdemo

# Build stage
FROM golang:latest AS builder

# Install ICU development libraries (required for dolt)
RUN apt-get update && apt-get install -y \
    libicu-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy doltswarm library (includes integration/)
COPY doltswarm /build/doltswarm

# Copy dolt and doltsqldriver (required by replace directives in go.mod)
COPY dolt /build/dolt
COPY doltsqldriver /build/doltsqldriver

WORKDIR /build/doltswarm/integration

# Download dependencies (skip checksum for private repos)
ENV GONOSUMDB=github.com/nustiueudinastea/*
RUN go mod download

# Build the binary
RUN go build -o ddolt .

# Runtime stage - use trixie to match golang:latest ICU version (76)
FROM debian:trixie-slim

# Install ICU runtime libraries (required for dolt)
RUN apt-get update && apt-get install -y \
    libicu76 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/doltswarm/integration/ddolt /app/ddolt

# Default environment variables
ENV PORT=10500
ENV LISTEN_ADDR=0.0.0.0
ENV DB_PATH=/data
ENV LOG_LEVEL=info

# Expose the default port (can be overridden)
EXPOSE 10500

# Use entrypoint script for flexibility
COPY doltswarm/integration/docker/docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["server"]
