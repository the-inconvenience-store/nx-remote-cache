package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// errStalled is the storage failure of an upload that storage stopped taking.
var errStalled = errors.New("S3 transfer stalled")

// side is the end of a stalled transfer that the server was waiting on.
type side string

const (
	sideStorage side = "storage"
	sideClient  side = "client"
)

// transfer is the artifact body the server receives: the S3 object of a
// download, or the client's request body of an upload. Each read that returns
// bytes restarts the stall clock. If the clock reaches the stall timeout, abort
// runs once to stop the transfer at both ends. transfer also records a read
// failure, so the handler can tell which end failed. The SDK reads an upload
// body from its transport goroutine.
type transfer struct {
	src     io.Reader
	timeout time.Duration
	timer   *time.Timer

	mu       sync.Mutex
	readErr  error
	finished bool
	stalled  bool
}

// startTransfer starts the stall clock for src. The caller must call finish.
func startTransfer(src io.Reader, timeout time.Duration, abort func()) *transfer {
	t := &transfer{src: src, timeout: timeout}
	t.timer = time.AfterFunc(timeout, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.finished || t.stalled {
			return
		}
		t.stalled = true
		abort()
	})
	return t
}

func (t *transfer) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > 0 && !t.finished && !t.stalled {
		t.timer.Reset(t.timeout)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.readErr = err
	}
	return n, err //nolint:wrapcheck // io.Reader contract requires the raw error, including io.EOF
}

// finish stops the stall clock and reports whether the transfer stalled. Once
// finish returns, abort does not run.
func (t *transfer) finish() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finished = true
	t.timer.Stop()
	return t.stalled
}

func (t *transfer) failed() bool {
	return t.err() != nil
}

func (t *transfer) err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readErr
}

// expireClientDeadline sets the deadline of the client connection with set,
// so a read or write blocked on a client that stopped is released.
// http.ResponseController supports deadlines on every server connection, so
// the only error is for a writer without a connection, which never blocks.
func expireClientDeadline(set func(time.Time) error) {
	_ = set(time.Now())
}

// reportStall logs and counts a transfer that the stall timeout stopped.
func (h *handler) reportStall(r *http.Request, hash string, waitingOn side) {
	level := slog.LevelWarn
	if waitingOn == sideStorage {
		level = slog.LevelError
	}
	h.log.Log(r.Context(), level, "Transfer stalled",
		"method", r.Method, "hash", hash, "side", string(waitingOn), "timeout", h.cfg.StallTimeout)
	h.metrics.stalls.WithLabelValues(r.Method, string(waitingOn)).Inc()
}
