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

// countingProxy forwards TCP connections to an S3 backend and counts the
// connections the server opens and the requests it sends. While held, it
// withholds responses, so every request of a burst is in flight at once.
type countingProxy struct {
	URL      string
	target   string
	opened   atomic.Int64
	requests atomic.Int64

	mu       sync.Mutex
	gate     chan struct{}
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
	p := &countingProxy{URL: "http://" + listener.Addr().String(), target: target.Host, gate: make(chan struct{})}
	close(p.gate)
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

// hold withholds responses until release.
func (p *countingProxy) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate = make(chan struct{})
}

func (p *countingProxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.gate:
	default:
		close(p.gate)
	}
}

func (p *countingProxy) currentGate() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gate
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
		// A bodiless GET arrives in one read: count each as a request.
		copyChunks(upstream, client, func() { p.requests.Add(1) })
		_ = upstream.Close()
	}()
	copyChunks(client, upstream, func() { <-p.currentGate() })
}

// copyChunks copies src to dst, calling before ahead of each chunk.
func copyChunks(dst io.Writer, src io.Reader, before func()) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			before()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestParallelDownloadsReuseStorageConnections sends the burst of parallel GETs
// Nx makes at the start of a run, twice, holding storage responses until every
// GET of a burst is in flight. The first burst needs one S3 connection per GET;
// the second must reuse them instead of dialling again.
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

	// heldBurst sends downloadBurst parallel GETs and returns how many new S3
	// connections they opened.
	heldBurst := func() int64 {
		openedBefore, requestsBefore := proxy.opened.Load(), proxy.requests.Load()
		proxy.hold()
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
		deadline := time.Now().Add(heldBurstDeadline)
		for proxy.requests.Load()-requestsBefore < downloadBurst && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		inFlight := proxy.requests.Load() - requestsBefore
		proxy.release()
		wg.Wait()
		if inFlight < downloadBurst {
			t.Fatalf("only %d of %d GETs reached storage while responses were held", inFlight, downloadBurst)
		}
		return proxy.opened.Load() - openedBefore
	}

	first := heldBurst()
	second := heldBurst()
	t.Logf("new S3 connections per burst of %d parallel GETs: first %d, second %d", downloadBurst, first, second)
	if second > downloadBurst/10 {
		t.Fatalf("second burst of %d parallel GETs opened %d new S3 connections; want far fewer",
			downloadBurst, second)
	}
}
