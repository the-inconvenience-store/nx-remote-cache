// Command nx-remote-cache serves the Nx self-hosted remote cache API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	// shutdownTimeout drains in-flight requests within Kubernetes' default 30s grace period.
	shutdownTimeout = 25 * time.Second
	maxPort         = 65535
)

var (
	errPort           = errors.New("PORT must be an integer between 0 and 65535")
	errFailOpen       = errors.New(`NX_CACHE_FAIL_OPEN must be "true" or "false"`)
	errForcePathStyle = errors.New(`S3_FORCE_PATH_STYLE must be "true" or "false"`)
	errTLSPair        = errors.New("TLS misconfiguration")
)

type lookupFunc func(key string) (string, bool)

type settings struct {
	port   int
	server server.Config
	tls    *tls.Config
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.LookupEnv, os.Stderr)
	stop()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, lookup lookupFunc, logOut io.Writer) error {
	s, err := loadSettings(lookup)
	if err != nil {
		return err
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", ":"+strconv.Itoa(s.port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.port, err)
	}
	return serve(ctx, &s, listener, logOut)
}

func loadSettings(lookup lookupFunc) (settings, error) {
	port, err := parsePort(valueOr(lookup, "PORT", "3000"))
	if err != nil {
		return settings{}, err
	}
	maxArtifactSize, err := parseMaxArtifactSize(lookup)
	if err != nil {
		return settings{}, err
	}
	failOpen, err := parseFailOpen(lookup)
	if err != nil {
		return settings{}, err
	}
	addressStyle, err := parseAddressStyle(lookup)
	if err != nil {
		return settings{}, err
	}
	tlsConfig, err := loadTLS(lookup)
	if err != nil {
		return settings{}, err
	}
	access, _ := lookup("NX_CACHE_ACCESS_TOKEN")
	var readOnlyTokens []string
	if readOnly, ok := lookup("NX_CACHE_READ_ONLY_TOKEN"); ok {
		readOnlyTokens = splitTokens(readOnly)
	}
	endpoint, _ := lookup("S3_ENDPOINT_URL")
	keyID, _ := lookup("AWS_ACCESS_KEY_ID")
	secret, _ := lookup("AWS_SECRET_ACCESS_KEY")
	return settings{
		port: port,
		tls:  tlsConfig,
		server: server.Config{
			AccessTokens:    splitTokens(access),
			ReadOnlyTokens:  readOnlyTokens,
			Bucket:          valueOr(lookup, "S3_BUCKET_NAME", "nx-cloud"),
			Region:          valueOr(lookup, "AWS_REGION", "us-east-1"),
			Endpoint:        endpoint,
			AddressStyle:    addressStyle,
			AccessKeyID:     keyID,
			SecretAccessKey: secret,
			FailOpen:        failOpen,
			MaxArtifactSize: maxArtifactSize,
		},
	}, nil
}

// splitTokens splits a comma-separated token list and removes the spaces
// around each comma. Empty items stay in the list so that validation rejects them.
func splitTokens(list string) []string {
	tokens := strings.Split(list, ",")
	last := len(tokens) - 1
	for i := range tokens {
		if i > 0 {
			tokens[i] = strings.TrimLeft(tokens[i], " ")
		}
		if i < last {
			tokens[i] = strings.TrimRight(tokens[i], " ")
		}
	}
	return tokens
}

// valueOr treats unset and empty variables alike, matching the original server.
func valueOr(lookup lookupFunc, key, fallback string) string {
	if v, _ := lookup(key); v != "" {
		return v
	}
	return fallback
}

func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 0 || port > maxPort {
		return 0, errPort
	}
	return port, nil
}

var errMaxArtifactSize = errors.New("NX_CACHE_MAX_ARTIFACT_BYTES must be a whole number of 0 or more")

// parseMaxArtifactSize returns 0 (no limit) only when the variable is unset;
// any value must be a whole number of 0 or more.
func parseMaxArtifactSize(lookup lookupFunc) (int64, error) {
	value, ok := lookup("NX_CACHE_MAX_ARTIFACT_BYTES")
	if !ok {
		return 0, nil
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size < 0 {
		return 0, errMaxArtifactSize
	}
	return size, nil
}

// parseFailOpen defaults only when the variable is unset; an empty value is an error.
func parseFailOpen(lookup lookupFunc) (bool, error) {
	value, ok := lookup("NX_CACHE_FAIL_OPEN")
	if !ok {
		return true, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errFailOpen
	}
}

// parseAddressStyle keeps the server's endpoint-based default when the
// variable is unset; an empty value is an error.
func parseAddressStyle(lookup lookupFunc) (server.AddressStyle, error) {
	value, ok := lookup("S3_FORCE_PATH_STYLE")
	if !ok {
		return server.AddressStyleAuto, nil
	}
	switch value {
	case "true":
		return server.AddressStylePath, nil
	case "false":
		return server.AddressStyleVirtualHosted, nil
	default:
		return server.AddressStyleAuto, errForcePathStyle
	}
}

func loadTLS(lookup lookupFunc) (*tls.Config, error) {
	certPath, _ := lookup("TLS_CERT_PATH")
	keyPath, _ := lookup("TLS_KEY_PATH")
	if certPath == "" && keyPath == "" {
		return nil, nil //nolint:nilnil // no TLS configured is a valid, non-error state
	}
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("%w: TLS_CERT_PATH and TLS_KEY_PATH must be set together", errTLSPair)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read cert/key: %w", errTLSPair, err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

func serve(ctx context.Context, s *settings, listener net.Listener, logOut io.Writer) error {
	logger := slog.New(slog.NewTextHandler(logOut, nil))
	s.server.Logger = logger
	handler, err := server.New(ctx, &s.server)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("configure server: %w", err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	if s.tls != nil {
		listener = tls.NewListener(listener, s.tls)
	}
	logger.InfoContext(ctx, "Server running", "addr", listener.Addr().String(), "https", s.tls != nil)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "Shutting down; draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
