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

func (h *handler) download(w http.ResponseWriter, r *http.Request, req *cacheRequest) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	timer := time.AfterFunc(headersTimeout, cancel)
	out, err := h.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &h.cfg.Bucket, Key: &req.hash})
	if !timer.Stop() {
		if err == nil {
			_ = out.Body.Close()
		}
		req.outcome = h.storageFailure(w, r, errHeadersTimeout)
		return
	}
	if err != nil {
		req.outcome = h.downloadFailed(w, r, err)
		return
	}
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

func (h *handler) downloadFailed(w http.ResponseWriter, r *http.Request, err error) outcome {
	if httpStatus(err) != http.StatusNotFound {
		return h.storageFailure(w, r, err)
	}
	// S3 also answers 404 for a missing bucket; only a missing key is a miss.
	if apiErrorCode(err) == "NoSuchBucket" {
		return h.storageFailure(w, r, errBucketNotFound)
	}
	writeText(w, http.StatusNotFound, "The record was not found")
	return outcomeMiss
}
