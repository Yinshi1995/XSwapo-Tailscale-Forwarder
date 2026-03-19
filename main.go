// go-tailscale-forwarder — minimal HTTPS reverse proxy via tsnet.
//
// The application joins your Tailscale network as an embedded node (no
// tailscaled sidecar), accepts HTTPS on :443 with a Tailscale-issued cert,
// and forwards every request to an internal upstream over plain HTTP.
//
// Required env:  UPSTREAM_URL  e.g. http://admin.railway.internal:8080
// Optional env:  TS_HOSTNAME, TS_STATE_DIR, TS_AUTHKEY, LISTEN_ADDR
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

// envOr returns the value of the environment variable key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// mustEnv returns the value of the environment variable key or calls log.Fatal.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %q is not set", key)
	}
	return v
}

func main() {
	// ── Configuration ─────────────────────────────────────────────────────────
	hostname    := envOr("TS_HOSTNAME", "godmode-xswapo-io")
	stateDir    := envOr("TS_STATE_DIR", "/data/tsnet")
	authKey     := os.Getenv("TS_AUTHKEY")  // optional once persisted state exists
	upstreamRaw := mustEnv("UPSTREAM_URL")  // e.g. http://admin.railway.internal:8080
	listenAddr  := envOr("LISTEN_ADDR", ":443")

	// Validate upstream URL early so we fail fast before touching Tailscale.
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Host == "" {
		log.Fatalf("invalid UPSTREAM_URL %q: %v", upstreamRaw, err)
	}

	// Ensure the tsnet state directory exists before starting the node.
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		log.Fatalf("cannot create state dir %q: %v", stateDir, err)
	}

	// ── Tailscale node (tsnet) ─────────────────────────────────────────────────
	// tsnet runs a full Tailscale node inside the process — no daemon required.
	// On the very first start without an AuthKey it prints a login URL via Logf.
	srv := &tsnet.Server{
		Hostname: hostname,
		Dir:      stateDir,
		AuthKey:  authKey,
		Logf:     log.Printf,
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		log.Fatalf("tsnet start: %v", err)
	}
	log.Printf("tsnet: node %q joined the tailnet", hostname)

	// LocalClient gives access to in-process Tailscale APIs:
	//   GetCertificate — Tailscale-issued TLS cert for <hostname>.ts.net
	//   WhoIs          — identity of the connecting Tailscale node
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("tsnet local client: %v", err)
	}

	// ── Listener ──────────────────────────────────────────────────────────────
	// srv.Listen binds on the Tailscale virtual interface, not on the host NIC.
	// The resulting listener is only reachable from inside the tailnet.
	ln, err := srv.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("tsnet listen %s: %v", listenAddr, err)
	}
	defer ln.Close()

	// Wrap with TLS when listening on :443.
	// Tailscale issues a valid ACME certificate for <hostname>.<tailnet>.ts.net.
	// Prerequisite: "HTTPS certificates" must be enabled in the Tailscale admin
	// console under Settings → DNS.
	scheme := "http"
	if listenAddr == ":443" {
		scheme = "https"
		ln = tls.NewListener(ln, &tls.Config{
			GetCertificate: lc.GetCertificate,
		})
		log.Printf("tsnet: TLS enabled — certificate for %s.ts.net", hostname)
	}

	// ── Reverse proxy ──────────────────────────────────────────────────────────
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}

	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		// Capture the original Host before defaultDirector rewrites req.URL
		// so we can pass it as X-Forwarded-Host.
		origHost := req.Host
		if origHost == "" {
			origHost = req.URL.Host
		}

		// Apply the default director: sets req.URL to the upstream target.
		defaultDirector(req)

		// Override the Host header so the upstream sees its own hostname,
		// not the tailnet hostname.
		req.Host = upstream.Host

		// Forwarding headers that the upstream may use for correct URL generation.
		req.Header.Set("X-Forwarded-Host", origHost)
		req.Header.Set("X-Forwarded-Proto", scheme)

		// Note: X-Forwarded-For (client Tailscale IP) is appended automatically
		// by ReverseProxy.ServeHTTP after this Director returns.
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error [%s %s]: %v", r.Method, r.URL.Path, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}

	// ── HTTP handler ──────────────────────────────────────────────────────────
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the Tailscale identity of the connecting node (best-effort).
		// A failure here is non-fatal — we still proxy the request.
		who, whoErr := lc.WhoIs(r.Context(), r.RemoteAddr)
		if whoErr == nil && who != nil {
			var login, device string
			if who.UserProfile != nil {
				login = who.UserProfile.LoginName
			}
			if who.Node != nil {
				device = who.Node.Name
			}
			log.Printf("[%s] %s %s  user=%q node=%q",
				r.RemoteAddr, r.Method, r.URL.Path, login, device)
		} else {
			log.Printf("[%s] %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		}

		proxy.ServeHTTP(w, r)
	})

	// ── HTTP server ───────────────────────────────────────────────────────────
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown: wait for SIGINT / SIGTERM, then drain in-flight requests.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("signal %s: shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("proxy ready: %s → %s  (listen %s)", scheme, upstream.Redacted(), listenAddr)
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	log.Printf("server stopped cleanly")
}
