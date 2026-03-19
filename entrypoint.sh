#!/bin/sh
set -e

# Railway mounts volumes as root at container startup, regardless of what
# the image baked in. Fix ownership of /data so the non-root app user
# can write the tsnet state, then drop privileges via su-exec.
chown -R app:app /data

exec su-exec app /app/forwarder "$@"
