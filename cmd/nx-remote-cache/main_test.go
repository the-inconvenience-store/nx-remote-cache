package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func env(values map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"NX_CACHE_ACCESS_TOKEN": "trusted",
		"AWS_ACCESS_KEY_ID":     "key",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"S3_ENDPOINT_URL":       "http://127.0.0.1:1",
	}
}

func TestLoadSettingsAppliesDefaults(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if s.port != 3000 || s.server.Region != "us-east-1" || s.server.Bucket != "nx-cloud" ||
		!s.server.FailOpen || s.server.ReadOnlyToken != "" || s.tls != nil {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestLoadSettingsRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"bad port":            {"PORT", "http", errPort},
		"port out of range":   {"PORT", "70000", errPort},
		"bad fail-open":       {"NX_CACHE_FAIL_OPEN", "maybe", errFailOpen},
		"empty fail-open":     {"NX_CACHE_FAIL_OPEN", "", errFailOpen},
		"empty read token":    {"NX_CACHE_READ_ONLY_TOKEN", "", server.ErrReadOnlyToken},
		"cert without key":    {"TLS_CERT_PATH", "testdata/cert.pem", errTLSPair},
		"unreadable key pair": {"TLS_KEY_PATH", "missing.pem", errTLSPair},
	}
	for name, tc := range cases {
		values := validEnv()
		values[tc.key] = tc.value
		if name == "unreadable key pair" {
			values["TLS_CERT_PATH"] = "testdata/cert.pem"
		}
		if _, err := loadSettings(env(values)); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestRunRejectsInvalidServerConfiguration(t *testing.T) {
	t.Parallel()
	values := validEnv()
	values["NX_CACHE_ACCESS_TOKEN"] = ""
	err := run(context.Background(), env(values), io.Discard)
	if !errors.Is(err, server.ErrAccessToken) {
		t.Fatalf("got %v, want %v", err, server.ErrAccessToken)
	}
}

func TestServeAnswersHealthAndShutsDownGracefully(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	assertServes(t, &s, http.DefaultClient, "http")
}

func TestServeUsesTLSWhenConfigured(t *testing.T) {
	t.Parallel()
	values := validEnv()
	values["TLS_CERT_PATH"] = "testdata/cert.pem"
	values["TLS_KEY_PATH"] = "testdata/key.pem"
	s, err := loadSettings(env(values))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile("testdata/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
	}
	assertServes(t, &s, client, "https")
}

func assertServes(t *testing.T, s *settings, client *http.Client, scheme string) {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T is not TCP", listener.Addr())
	}
	port := addr.Port
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, listener, io.Discard) }()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		scheme+"://localhost:"+strconv.Itoa(port)+"/health", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "OK" {
		t.Fatalf("health: %d %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
}
