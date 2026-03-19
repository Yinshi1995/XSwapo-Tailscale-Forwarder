# go-tailscale-forwarder

Minimal, production-ready Go reverse proxy that joins your Tailscale network as
an embedded node, terminates HTTPS on port 443 with a Tailscale-issued
certificate, and forwards all traffic to an internal upstream (e.g. a Railway
private service).

No Caddy, no nginx, no tailscaled sidecar, no docker-compose. Just a single Go
binary.

---

## 1. What the Project Does

`go-tailscale-forwarder` starts a [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)
node — a full Tailscale node embedded directly inside the Go process. Once the
node is connected, the application:

1. Listens on TCP `:443` **inside the tailnet** (not on the public internet).
2. Wraps the listener with TLS using a certificate issued by Tailscale/Let's
   Encrypt for `<TS_HOSTNAME>.<tailnet>.ts.net`.
3. Proxies every HTTPS request to an internal upstream over plain HTTP using
   `httputil.ReverseProxy`.
4. Logs the Tailscale identity (login name, node name) of every connecting peer.

---

## 2. How the Scheme Works

```
Browser / VPN peer  (in tailnet)
        │
        │  HTTPS  ← TLS terminated HERE, cert from Tailscale
        ▼
┌─────────────────────────────────────────┐
│          go-tailscale-forwarder         │
│                                         │
│  ┌─────────────────────────────────┐   │
│  │  tsnet — embedded Tailscale     │   │
│  │  node (WireGuard, DERP, etc.)   │   │
│  └─────────────────────────────────┘   │
│  ┌─────────────────────────────────┐   │
│  │  tls.NewListener + lc.GetCert   │   │
│  └─────────────────────────────────┘   │
│  ┌─────────────────────────────────┐   │
│  │  httputil.ReverseProxy          │   │
│  └─────────────────────────────────┘   │
└─────────────────────────────────────────┘
        │
        │  HTTP  (Railway private network)
        ▼
┌─────────────────────────────────────────┐
│  upstream service                       │
│  http://admin.railway.internal:8080     │
└─────────────────────────────────────────┘
```

### Why a Plain TCP Forwarder Is Not Enough

A TCP forwarder (e.g. `socat`, `ssh -L`, a raw Go `io.Copy` loop) transparently
tunnels bytes. That means the TLS handshake is forwarded **as-is** to the
upstream — but the upstream holds no certificate for
`<hostname>.<tailnet>.ts.net`, so the browser rejects the connection.

TLS termination requires the proxy to hold the certificate and private key, and
be able to answer the TLS ClientHello. This only works when the Tailscale node
runs inside the application itself (via `tsnet`), which grants access to
`LocalClient.GetCertificate` — the in-process Tailscale certificate store.

### Why `tsnet` Specifically

`tsnet` embeds the entire Tailscale node in-process:
- No kernel module or `tailscaled` daemon required.
- No privileged operations.
- Compatible with any Linux container (including Railway with no host network
  access).
- `LocalClient` gives direct access to `GetCertificate` and `WhoIs` without
  going through a UNIX socket.

---

## 3. Environment Variables

| Variable        | Default             | Required | Description |
|-----------------|---------------------|----------|-------------|
| `UPSTREAM_URL`  | —                   | **Yes**  | HTTP address of the internal upstream, e.g. `http://admin.railway.internal:8080` |
| `TS_HOSTNAME`   | `godmode-xswapo-io` | No       | Tailscale node hostname (must be unique in your tailnet) |
| `TS_STATE_DIR`  | `/data/tsnet`       | No       | Directory where tsnet persists keys and node identity |
| `TS_AUTHKEY`    | —                   | No*      | Tailscale auth key for headless first-time login |
| `LISTEN_ADDR`   | `:443`              | No       | TCP listen address inside the tailnet |

\* `TS_AUTHKEY` is required the first time the container starts (or whenever
`TS_STATE_DIR` is empty). After the node has logged in and state has been
persisted to the volume, the container can restart without it.

Use a **reusable, non-ephemeral** auth key so the node survives restarts:
```
tskey-auth-<REDACTED>?preauthorized=true&ephemeral=false
```
Or tag the key so ACL rules can target it:
```
tskey-auth-<REDACTED>?preauthorized=true&ephemeral=false&tags=tag:proxy
```

### Prerequisite: Enable HTTPS Certificates

Before the proxy can serve HTTPS, go to your Tailscale admin console:

**Settings → DNS → Enable HTTPS certificates**

Without this, `lc.GetCertificate` will fail and no TLS certificate will be
issued.

---

## 4. Local Development

```bash
git clone https://github.com/slava/go-tailscale-forwarder
cd go-tailscale-forwarder

export UPSTREAM_URL=http://localhost:8080
export TS_HOSTNAME=my-dev-proxy
export TS_STATE_DIR=./tsnet-state
export TS_AUTHKEY=tskey-auth-...

go run .
```

On the first run (or when `TS_STATE_DIR` is empty and `TS_AUTHKEY` is unset),
the log will print a URL — open it in a browser to authenticate the node.

After authentication succeed, the log will print:
```
tsnet: node "my-dev-proxy" joined the tailnet
tsnet: TLS enabled — certificate for my-dev-proxy.ts.net
proxy ready: https → http://localhost:8080  (listen :443)
```

---

## 5. Docker Build

```bash
docker build -t go-tailscale-forwarder .

# Run locally (maps host :8443 → container :443 for testing)
docker run --rm \
  -e UPSTREAM_URL=http://host.docker.internal:8080 \
  -e TS_HOSTNAME=my-proxy \
  -e TS_AUTHKEY=tskey-auth-... \
  -v tsnet-data:/data \
  -p 8443:443 \
  go-tailscale-forwarder
```

---

## 6. Deploy to Railway

### Step 1 — Create a New Service

Connect your GitHub repository or push the Docker image. Railway will
auto-detect the `Dockerfile`.

### Step 2 — Set Environment Variables

In the Railway service settings → Variables:

```
TS_HOSTNAME=godmode-xswapo-io
TS_STATE_DIR=/data/tsnet
TS_AUTHKEY=tskey-auth-...
UPSTREAM_URL=http://admin.railway.internal:8080
LISTEN_ADDR=:443
```

### Step 3 — Attach a Persistent Volume

Go to **Service → Volumes** and add a volume mounted at `/data`.

This is critical. Without it:
- The Tailscale node re-registers on every deploy, consuming auth keys.
- The node loses its stable Tailscale IP and identity.
- The HTTPS certificate must be re-issued on every start.

With a persistent volume, the node reconnects on restart with minimal delay.

### Step 4 — No Public Port Required

Do **not** add a Railway public domain or TCP proxy for port 443. This service
is only reachable from inside the tailnet. Tailscale handles all routing through
its WireGuard mesh — no public exposure is needed.

> If Railway complains about the absence of a health-check port, add a second
> listener on `:8080` that responds with `{"ok":true}`. Alternatively, disable
> the health check in Railway settings.

### Step 5 — Deploy

Trigger a deploy. Watch the logs:
```
tsnet: node "godmode-xswapo-io" joined the tailnet
tsnet: TLS enabled — certificate for godmode-xswapo-io.ts.net
proxy ready: https → http://admin.railway.internal:8080  (listen :443)
```

---

## 7. What URL to Open

Once deployed and the node is visible in your Tailscale admin console:

```
https://<TS_HOSTNAME>.<tailnet>.ts.net
```

For example:
```
https://godmode-xswapo-io.funky-owl.ts.net
```

This URL works from any device in your tailnet (laptop, phone, CI runner, etc.)
that has Tailscale running.

---

## 8. Why This Solves the HTTPS Problem in Tailnet

| Problem | Solution |
|---------|----------|
| Railway internal services are HTTP-only | The proxy adds TLS at the tailnet boundary |
| Self-signed certs trigger browser warnings | Tailscale issues a trusted ACME cert via Let's Encrypt |
| Public HTTPS exposure is undesirable | Port 443 is only reachable in the tailnet |
| Caddy/nginx require extra config and a daemon | This is a single Go binary with no sidecar |
| Auth key rotation on every restart | Persistent volume preserves tsnet state across deploys |

---

## 9. Limitations

- **One upstream per instance.** To proxy multiple internal services, deploy
  multiple instances with different `TS_HOSTNAME` and `UPSTREAM_URL` values.
- **HTTP/1.1 only.** WebSocket upgrades and gRPC streaming are not tested.
  Add `proxy.Transport` customisations if needed.
- **Tailscale device limit.** Each deployment counts as one node in your
  tailnet. Free plans have a device limit.
- **Ephemeral keys.** If `TS_AUTHKEY` is ephemeral, the node is removed from
  the tailnet when the container stops. Use a non-ephemeral key with a
  persistent volume.
- **Certificate issuance latency.** The first HTTPS request after a fresh
  start may be slow (~3 s) while Tailscale provisions the certificate.

---

## 10. Security Hardening Ideas

- **Tailscale ACL rules.** Restrict which tailnet nodes can reach TCP port 443
  on this node. A minimal policy:
  ```json
  {"action": "accept", "src": ["tag:trusted"], "dst": ["tag:proxy:443"]}
  ```
- **Tag-based auth keys.** Tag the node so ACL rules can reference it by role
  rather than identity:
  ```
  tskey-auth-...?tags=tag:proxy
  ```
- **Identity enforcement.** Extend the HTTP handler to reject requests where
  `lc.WhoIs` returns an unknown or untrusted user/tag.
- **Rate limiting.** Add a `golang.org/x/time/rate` token bucket per Tailscale
  node IP before calling `proxy.ServeHTTP`.
- **Read-only filesystem.** Mount the container filesystem read-only in Railway
  (`--read-only`), with only `/data` as a writable volume.
- **Request ID header.** Inject `X-Request-ID` in the Director and log it for
  end-to-end tracing.
- **Upstream mTLS.** If the Railway upstream supports it, configure the
  `proxy.Transport` with a client certificate for mutual TLS.
