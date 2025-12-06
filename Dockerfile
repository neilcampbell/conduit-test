# Multi-stage build for conduit with LocalNet algod importer plugin

# Stage 1: Build the conduit binary
FROM debian:bullseye-slim AS builder

# Install build dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    wget \
    ca-certificates \
    git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Install Go 1.25.3
ARG GOLANG_VERSION=1.25.3
ARG TARGETARCH
ENV GOROOT=/usr/local/go
ENV GOPATH=/root/go
ENV PATH=$GOPATH/bin:$GOROOT/bin:$PATH
ENV CGO_ENABLED=0

RUN wget --quiet https://dl.google.com/go/go${GOLANG_VERSION}.linux-${TARGETARCH}.tar.gz && \
    echo "$(wget --quiet -O - https://dl.google.com/go/go${GOLANG_VERSION}.linux-${TARGETARCH}.tar.gz.sha256) go${GOLANG_VERSION}.linux-${TARGETARCH}.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf go${GOLANG_VERSION}.linux-${TARGETARCH}.tar.gz && \
    rm go${GOLANG_VERSION}.linux-${TARGETARCH}.tar.gz

# Verify Go installation
RUN go version

WORKDIR /build

# Copy go module files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies (cached if go.mod/go.sum unchanged)
RUN go mod download && \
    go mod verify

# Copy source code
COPY . .

# Build with security flags and optimizations
RUN go mod tidy && \
    go build \
    -ldflags='-w -s -extldflags "-static"' \
    -trimpath \
    -o conduit \
    cmd/conduit/main.go && \
    chmod +x conduit

# Verify binary
RUN /build/conduit -v

# Clean up build cache and temporary files
RUN go clean -cache -modcache -testcache && \
    rm -rf /root/.cache /tmp/* /var/tmp/*

# Stage 2: Runtime image
FROM debian:bullseye-slim

# Metadata labels
LABEL org.opencontainers.image.title="Conduit LocalNet" \
      org.opencontainers.image.description="Algorand Conduit with LocalNet algod importer plugin" \
      org.opencontainers.image.vendor="Algorand Foundation" \
      org.opencontainers.image.source="https://github.com/neilcampbell/conduit-localnet-importer" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.documentation="https://github.com/neilcampbell/conduit-localnet-importer/blob/main/README.md"

# Hard code UID/GID to 999 for consistency in advanced deployments.
# Install ca-certificates to enable using infra providers.
# Install gosu for fancy data directory management.
RUN groupadd --gid=999 --system algorand && \
    useradd --uid=999 --no-log-init --create-home --system --gid algorand algorand && \
    mkdir -p /data && \
    chown -R algorand:algorand /data && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    gosu \
    ca-certificates \
    && update-ca-certificates && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /var/cache/apt/archives/*

# Copy the built binary from builder stage with ownership
COPY --from=builder --chown=root:root --chmod=755 /build/conduit /usr/local/bin/conduit

# Copy docker entrypoint script with ownership
COPY --chown=root:root --chmod=755 docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ENV CONDUIT_DATA_DIR=/data
WORKDIR ${CONDUIT_DATA_DIR}

# Note: docker-entrypoint.sh calls 'conduit'. Similar entrypoint scripts
# accept the binary as the first argument in order to surface a suite of
# tools (i.e. algod, goal, algocfg, ...). Maybe this will change in the
# future, but for now this approach seemed simpler.
ENTRYPOINT ["docker-entrypoint.sh"]
