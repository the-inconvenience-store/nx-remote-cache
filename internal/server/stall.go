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
// download, or the client's request body of an upload. Bytes moving on either
// side restart the stall clock: a read starts once the reader has passed the
// previous chunk on, and returns with the next chunk. If the clock reaches the
// stall timeout, abort runs once to stop the transfer at both ends. transfer
// also records a read failure, so the handler can tell which end failed. The
// SDK reads an upload body from its transport goroutine.
type transfer struct {
	src     io.Reader
	timeout time.Duration
	abort   func()

	mu sync.Mutex
	// timer fires at the earliest time the transfer can have stalled; expire
	// checks progress before stopping it.
	timer    *time.Timer
	progress time.Time
	readErr  error
	finished bool
	stalled  bool
}

// startTransfer starts the stall clock for src. The caller must call finish.
func startTransfer(src io.Reader, timeout time.Duration, abort func()) *transfer {
	t := &transfer{src: src, timeout: timeout, abort: abort, progress: time.Now()}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timer = time.AfterFunc(timeout, t.expire)
	return t
}

// expire stops the transfer if no bytes moved for the timeout, and otherwise
// sets the timer for the timeout after the latest progress.
func (t *transfer) expire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if idle := time.Since(t.progress); idle < t.timeout {
		t.timer.Reset(t.timeout - idle)
		return
	}
	t.stalled = true
	t.abort()
}

func (t *transfer) moved() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress = time.Now()
}

func (t *transfer) Read(p []byte) (int, error) {
	t.moved()
	n, err := t.src.Read(p)
	if n > 0 {
		t.moved()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.mu.Lock()
		t.readErr = err
		t.mu.Unlock()
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
