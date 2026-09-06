package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
)

// A gateway is the assembled service: one HTTP handler and the process pool
// behind it, together with what it takes to stop both.
type gateway struct {
	handler  http.Handler
	pool     *pool.Pool
	addr     string
	shutdown time.Duration
	logger   *slog.Logger
}

// build turns the command line and the configuration file into the four layers
// the design calls for — authenticate, decide the policy, resolve the server
// set, serve it from the pool — stacked in that order.
//
// Everything that can fail fails here, before anything is listening: a
// deployment learns that its allowlist is missing or its key is unreadable at
// startup, not on the first request that needed it.
func build(ctx context.Context, opts options) (*gateway, error) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	base, err := config.Load(ctx, opts.config)
	if err != nil {
		return nil, err
	}
	// The policy is built before anything else that uses it, because it is
	// where a define-new deployment without a command allowlist is refused.
	p, err := policy.New(base.Policy)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", opts.config, err)
	}

	keys, err := loadKeys(opts.keys)
	if err != nil {
		return nil, err
	}
	verifier, err := auth.NewVerifier(keys, opts.issuers, opts.resource)
	if err != nil {
		return nil, err
	}
	metadata, err := auth.NewMetadata(opts.resource, opts.issuers,
		append([]string{auth.ScopeAccess}, p.Scopes()...))
	if err != nil {
		return nil, err
	}
	metadataURL, err := auth.MetadataURL(opts.resource)
	if err != nil {
		return nil, err
	}

	// One fingerprinter for the process: its key is random per instance, so a
	// second one would give the same configuration a different fingerprint and
	// the pool would miss on every request.
	fingerprints, err := config.NewFingerprinter()
	if err != nil {
		return nil, err
	}
	version := buildVersion()
	// The pool outlives any one request and owns its own lifetime; ctx here is
	// startup's, and tying child processes to it would stop them the moment
	// configuration finished loading.
	//nolint:contextcheck // see above
	processes := pool.New(
		backend.NewConnector(version, base.Limits.ProbeTimeout.Duration(), logger),
		fingerprints, base.Limits, pool.DefaultStopPolicy, logger)

	endpoint := frontend.NewEndpoint(version, frontend.NewTenant(processes, 0, logger), logger)

	mux := http.NewServeMux()
	mux.Handle(auth.MetadataPath, metadata.Handler())
	mux.Handle(opts.path, auth.Require(verifier, metadataURL, logger)(
		// Claims are taken from the context the middleware above just filled,
		// so the token is verified once per request rather than twice.
		frontend.Resolve(base, p, nil, metadataURL, logger)(endpoint.Handler())))

	logger.Info("configured",
		"servers", len(base.Servers), "policy", p.Mode(),
		"maxProcesses", base.Limits.MaxProcesses, "resource", metadata.Resource)
	return &gateway{
		handler:  mux,
		pool:     processes,
		addr:     opts.addr,
		shutdown: opts.shutdown,
		logger:   logger,
	}, nil
}

// serve listens until the context ends, then stops taking requests, lets the
// ones in flight finish, and only then stops the backends.
//
// The order is the point. Stopping the pool first would kill the child
// processes out from under requests that are still being answered; the
// listener closing first means no new request can arrive to be disappointed.
func (g *gateway) serve(ctx context.Context, out io.Writer) error {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", g.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", g.addr, err)
	}
	// Written rather than only logged, because it is the one fact a caller
	// needs back: with -addr on port 0 this is how anybody learns where the
	// gateway actually is.
	if _, err := fmt.Fprintf(out, "listening on %s\n", listener.Addr()); err != nil {
		_ = listener.Close()
		return fmt.Errorf("report the listening address: %w", err)
	}

	server := &http.Server{
		Handler: g.handler,
		// A modern MCP stream is held open by design, so a read or write
		// deadline would cut off exactly the requests that are working. The
		// header deadline is the one that still bounds a client that connects
		// and says nothing.
		ReadHeaderTimeout: 10 * time.Second,
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Go(func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	})

	select {
	case err := <-errs:
		_ = g.pool.Close(context.WithoutCancel(ctx))
		wg.Wait()
		return err
	case <-ctx.Done():
	}

	g.logger.Info("shutting down", "grace", g.shutdown)
	// Detached from the cancelled context on purpose: the signal that started
	// the shutdown must not also be what cuts it short.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.shutdown)
	defer cancel()
	shutdownErr := server.Shutdown(grace)
	wg.Wait()
	poolErr := g.pool.Close(context.WithoutCancel(ctx))
	if err := errors.Join(shutdownErr, poolErr); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	g.logger.Info("stopped")
	return nil
}

// loadKeys reads the configured public keys into the source the verifier asks.
func loadKeys(files []keyFile) (auth.KeySource, error) {
	byIssuer := map[string]map[string]crypto.PublicKey{}
	for _, f := range files {
		// G304: a path from this deployment's own command line.
		data, err := os.ReadFile(f.path) //nolint:gosec // see above
		if err != nil {
			return nil, fmt.Errorf("key of issuer %q: %w", f.issuer, err)
		}
		key, err := parsePublicKey(data)
		if err != nil {
			return nil, fmt.Errorf("key %s of issuer %q: %w", f.path, f.issuer, err)
		}
		if byIssuer[f.issuer] == nil {
			byIssuer[f.issuer] = map[string]crypto.PublicKey{}
		}
		if _, taken := byIssuer[f.issuer][f.kid]; taken {
			return nil, fmt.Errorf("issuer %q has two keys with id %q", f.issuer, f.kid)
		}
		byIssuer[f.issuer][f.kid] = key
	}
	return auth.NewStaticKeys(byIssuer), nil
}

// parsePublicKey reads a PEM-encoded public key or certificate.
//
// Only the public half is accepted: a resource server verifies signatures and
// has no use for a private key, so reading one would mean somebody handed the
// gateway a secret it did not need.
func parsePublicKey(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not PEM-encoded")
	}
	switch block.Type {
	case "PUBLIC KEY":
		return x509.ParsePKIXPublicKey(block.Bytes)
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return cert.PublicKey, nil
	default:
		return nil, fmt.Errorf("PEM block %q is not a public key", block.Type)
	}
}

// buildVersion is what the gateway calls itself to clients and to backends.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}
