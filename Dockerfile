# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

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
# su-exec is a minimal setuid helper (like gosu) used in entrypoint.sh to
# drop from root to the app user after fixing volume ownership.
RUN apk add --no-cache ca-certificates su-exec

# Non-root user for principle of least privilege.
RUN addgroup -S app && adduser -S -G app app

# Pre-create /data so the directory exists even without a volume.
RUN mkdir -p /data

WORKDIR /app

# Copy the binary and the entrypoint script.
COPY --from=build /app/forwarder .
COPY entrypoint.sh .
RUN chmod +x entrypoint.sh

# /data must be backed by a persistent volume in Railway so the Tailscale
# node state (keys, identity) survives container restarts.
# Railway mounts this volume as root at runtime; entrypoint.sh fixes the
# ownership before exec-ing the binary as the app user.
VOLUME ["/data"]

# The proxy accepts HTTPS connections from within the tailnet on port 443.
# This port is NOT exposed to the public internet; all traffic goes through
# the Tailscale WireGuard tunnel.
EXPOSE 443

# Runs as root only long enough to chown /data, then drops to app via su-exec.
ENTRYPOINT ["/app/entrypoint.sh"]
