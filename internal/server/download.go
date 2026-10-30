package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// headersTimeout bounds only the wait for S3 response headers; the artifact
// body may stream for longer.
const headersTimeout = 5 * time.Second

// download serves the first of the credential's read locations that holds
// the artifact. A miss in one location moves on to the next; a storage
// failure answers at once.
func (h *handler) download(w http.ResponseWriter, r *http.Request, req *cacheRequest) {
	for _, loc := range req.cred.reads(req.hash) {
		out, cancel, err := h.getObject(r.Context(), loc.key)
		if err == nil {
			req.tier = loc.tier
			h.send(w, r, req, out, cancel)
			return
		}
		if httpStatus(err) == http.StatusNotFound && apiErrorCode(err) != "NoSuchBucket" {
			continue
		}
		// S3 also answers 404 for a missing bucket; only a missing key is a miss.
		if apiErrorCode(err) == "NoSuchBucket" {
			err = errBucketNotFound
		}
		req.outcome = h.storageFailure(w, r, err)
		return
	}
	writeText(w, http.StatusNotFound, "The record was not found")
	req.outcome = outcomeMiss
}

// getObject requests key and gives up if S3 sends no response headers within
// headersTimeout. On success the caller owns cancel, which stops the body.
func (h *handler) getObject(ctx context.Context, key string) (*s3.GetObjectOutput, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(headersTimeout, cancel)
	out, err := h.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &h.cfg.Bucket, Key: &key})
	if !timer.Stop() {
		if err == nil {
			_ = out.Body.Close()
		}
		cancel()
		return nil, nil, errHeadersTimeout
	}
	if err != nil {
		cancel()
		return nil, nil, err //nolint:wrapcheck // callers classify the SDK error
	}
	return out, cancel, nil
}

// send streams a found artifact to the client; cancel stops the S3 request.
func (h *handler) send(w http.ResponseWriter, r *http.Request, req *cacheRequest, out *s3.GetObjectOutput,
	cancel context.CancelFunc,
) {
	defer cancel()
	defer func() { _ = out.Body.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	if out.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	body := startTransfer(out.Body, h.cfg.StallTimeout, func() {
		cancel()
		expireClientDeadline(rc.SetWriteDeadline)
	})
	n, err := io.Copy(w, body)
	stalled := body.finish()
	req.bytes = n
	h.metrics.sent.Add(float64(n))
	// A stall that fired as the copy ended has already expired the client's
	// write deadline, so the response cannot be completed either.
	if err == nil && !stalled {
		req.outcome = outcomeHit
		return
	}
	req.outcome = outcomeError
	switch {
	case stalled && body.failed():
		h.reportStall(r, req.hash, sideStorage)
	case stalled:
		h.reportStall(r, req.hash, sideClient)
	default:
		h.log.ErrorContext(r.Context(), "Download interrupted", "hash", req.hash, "error", err)
	}
	// The status is already sent; abort so the client sees a truncated
	// response instead of a short artifact that looks complete.
	panic(http.ErrAbortHandler)
}
