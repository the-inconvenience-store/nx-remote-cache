package server

import (
	"errors"
	"io"
	"net/http"
	"sync"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
)

func (h *handler) upload(w http.ResponseWriter, r *http.Request, req *cacheRequest) {
	if req.access == accessRead {
		writeText(w, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
		req.outcome = outcomeReadOnly
		return
	}
	// Go reports a request with neither Content-Length nor Transfer-Encoding as
	// length 0, so the header itself must be present.
	if r.ContentLength < 0 || r.Header.Get("Content-Length") == "" {
		writeText(w, http.StatusLengthRequired, "Content-Length header is required")
		req.outcome = outcomeBadRequest
		return
	}
	if h.cfg.MaxArtifactSize > 0 && r.ContentLength > h.cfg.MaxArtifactSize {
		writeText(w, http.StatusForbidden, "Artifact exceeds the maximum size; artifact not stored")
		req.outcome = outcomeTooLarge
		return
	}

	exists, err := h.objectExists(r, req.hash)
	if err != nil {
		req.outcome = h.storageFailure(w, r, err)
		return
	}
	if exists {
		writeText(w, http.StatusConflict, "Cannot override an existing record")
		req.outcome = outcomeConflict
		return
	}
	req.outcome = h.putObject(w, r, req.hash)
}

// objectExists is a fast duplicate check. It is not race-free; the conditional
// PUT provides atomicity where storage supports it.
func (h *handler) objectExists(r *http.Request, hash string) (bool, error) {
	_, err := h.s3.HeadObject(r.Context(), &s3.HeadObjectInput{Bucket: &h.cfg.Bucket, Key: &hash})
	if err == nil {
		return true, nil
	}
	if httpStatus(err) == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (h *handler) putObject(w http.ResponseWriter, r *http.Request, hash string) outcome {
	body := &trackedBody{r: r.Body, received: h.metrics.received}
	input := &s3.PutObjectInput{
		Bucket:        &h.cfg.Bucket,
		Key:           &hash,
		Body:          body,
		ContentLength: new(r.ContentLength),
	}
	conditional := !h.conditionalUnsupported.Load()
	if conditional {
		input.IfNoneMatch = new("*")
	}
	// The request body streams straight to S3 and cannot be replayed, so the
	// payload is unsigned and the SDK must not retry.
	_, err := h.s3.PutObject(r.Context(), input,
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware),
		func(o *s3.Options) { o.RetryMaxAttempts = 1 })

	switch {
	case err == nil:
		writeText(w, http.StatusOK, "Successfully uploaded")
		return outcomeStored
	case body.failed():
		h.log.WarnContext(r.Context(), "Upload aborted by client", "hash", hash, "error", body.err())
		writeText(w, http.StatusInternalServerError, "Internal server error")
		return outcomeError
	case apiErrorCode(err) == "PreconditionFailed":
		writeText(w, http.StatusConflict, "Cannot override an existing record")
		return outcomeConflict
	case conditional && unsupportedCondition(err):
		h.conditionalUnsupported.Store(true)
		h.log.WarnContext(r.Context(),
			"Storage does not support conditional PUT; later uploads are not atomic",
			"hash", hash, "error", err)
		writeNotStored(w)
		return outcomeDegraded
	default:
		return h.storageFailure(w, r, err)
	}
}

// trackedBody counts the bytes it reads and records a client-side read failure
// so it is not mistaken for a storage failure. The SDK reads it from its
// transport goroutine.
type trackedBody struct {
	r        io.Reader
	received prometheus.Counter
	mu       sync.Mutex
	readErr  error
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.received.Add(float64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		b.mu.Lock()
		b.readErr = err
		b.mu.Unlock()
	}
	return n, err //nolint:wrapcheck // io.Reader contract requires the raw error, including io.EOF
}

func (b *trackedBody) failed() bool {
	return b.err() != nil
}

func (b *trackedBody) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readErr
}
