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

func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	if h.access(r) == accessNone {
		writeText(w, http.StatusUnauthorized, "Missing or invalid authentication token")
		return
	}
	hash := r.PathValue("hash")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	timer := time.AfterFunc(headersTimeout, cancel)
	out, err := h.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &h.cfg.Bucket, Key: &hash})
	if !timer.Stop() {
		if err == nil {
			_ = out.Body.Close()
		}
		h.storageFailure(w, r, errHeadersTimeout)
		return
	}
	if err != nil {
		h.downloadFailed(w, r, err)
		return
	}
	defer func() { _ = out.Body.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	if out.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, out.Body); err != nil {
		h.log.ErrorContext(r.Context(), "Download interrupted", "hash", hash, "error", err)
		// The status is already sent; abort so the client sees a truncated
		// response instead of a short artifact that looks complete.
		panic(http.ErrAbortHandler)
	}
}

func (h *handler) downloadFailed(w http.ResponseWriter, r *http.Request, err error) {
	if httpStatus(err) != http.StatusNotFound {
		h.storageFailure(w, r, err)
		return
	}
	// S3 also answers 404 for a missing bucket; only a missing key is a miss.
	if apiErrorCode(err) == "NoSuchBucket" {
		h.storageFailure(w, r, errBucketNotFound)
		return
	}
	writeText(w, http.StatusNotFound, "The record was not found")
}
