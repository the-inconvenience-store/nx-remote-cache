//go:build integration

package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/s3test"
)

const (
	downloadBurst = 200
	// heldBurstDeadline stays below the server's 5s wait for S3 response
	// headers, after which held GETs would degrade to cache misses.
	heldBurstDeadline = 4 * time.Second
)

// countingProxy forwards TCP connections to an S3 backend and counts how many
// the server opens. Responses wait until release, so a burst can be held open
// until every request has its own connection.
type countingProxy struct {
	URL      string
	target   string
	opened   atomic.Int64
	held     chan struct{}
	release  func()
	mu       sync.Mutex
	upstream []net.Conn
}

func startCountingProxy(t *testing.T, endpoint string) *countingProxy {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	p := &countingProxy{
		URL: "http://" + listener.Addr().String(), target: target.Host,
		held: held, release: sync.OnceFunc(func() { close(held) }),
	}
	t.Cleanup(func() {
		p.release()
		_ = listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, conn := range p.upstream {
			_ = conn.Close()
		}
	})
	go p.serve(listener)
	return p
}

func (p *countingProxy) serve(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		p.opened.Add(1)
		go p.forward(client)
	}
}

func (p *countingProxy) forward(client net.Conn) {
	defer func() { _ = client.Close() }()
	upstream, err := new(net.Dialer).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.upstream = append(p.upstream, upstream)
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	<-p.held
	_, _ = io.Copy(client, upstream)
}

// TestParallelDownloadsReuseStorageConnections sends the burst of parallel GETs
// Nx makes at the start of a run, twice. The first burst is held until it has
// opened one S3 connection per GET; the second must reuse those connections.
func TestParallelDownloadsReuseStorageConnections(t *testing.T) {
	t.Parallel()
	b := s3test.StartGarage(t)
	if _, err := b.Client().PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(s3test.Bucket), Key: aws.String("pooled"), Body: strings.NewReader("artifact"),
	}); err != nil {
		t.Fatal(err)
	}
	proxy := startCountingProxy(t, b.Endpoint)
	viaProxy := *b
	viaProxy.Endpoint = proxy.URL
	h := newBackendHandler(t, &viaProxy, backendServer{})

	burst := func() (wait func()) {
		var wg sync.WaitGroup
		for range downloadBurst {
			wg.Go(func() {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/cache/pooled", nil)
				req.Header.Set("Authorization", "Bearer "+accessToken)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Body.String() != "artifact" {
					t.Errorf("GET: %d %q", rec.Code, rec.Body.String())
				}
			})
		}
		return wg.Wait
	}

	waitFirst := burst()
	deadline := time.Now().Add(heldBurstDeadline)
	for proxy.opened.Load() < downloadBurst && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := proxy.opened.Load(); got != downloadBurst {
		t.Fatalf("first burst opened %d S3 connections, want one per GET (%d)", got, downloadBurst)
	}
	proxy.release()
	waitFirst()

	burst()()
	opened := proxy.opened.Load() - downloadBurst
	t.Logf("second burst of %d parallel GETs opened %d new S3 connections", downloadBurst, opened)
	if opened > downloadBurst/10 {
		t.Fatalf("second burst of %d parallel GETs opened %d new S3 connections; want far fewer",
			downloadBurst, opened)
	}
}
