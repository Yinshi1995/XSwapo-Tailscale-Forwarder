# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build

WORKDIR /src

# Download dependencies first to exploit layer caching.
# These layers are rebuilt only when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Build a fully static binary (no CGO, stripped debug info).
COPY . .
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /app/forwarder \
      .

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM alpine:3.21

# CA certificates are required for:
#   - TLS connections to the Tailscale control plane
#   - TLS connections to the upstream if it ever uses HTTPS
RUN apk add --no-cache ca-certificates

# Non-root user for principle of least privilege.
RUN addgroup -S app && adduser -S -G app app

# Create the tsnet state directory and hand ownership to the app user
# BEFORE the VOLUME declaration so the mount point is owned correctly.
RUN mkdir -p /data && chown app:app /data

WORKDIR /app

# Copy only the compiled binary; no source, no toolchain.
COPY --from=build --chown=app:app /app/forwarder .

# /data must be backed by a persistent volume in Railway so the Tailscale
# node state (keys, identity) survives container restarts.
VOLUME ["/data"]

# The proxy accepts HTTPS connections from within the tailnet on port 443.
# This port is NOT exposed to the public internet; all traffic goes through
# the Tailscale WireGuard tunnel.
EXPOSE 443

USER app

ENTRYPOINT ["/app/forwarder"]
