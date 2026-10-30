package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
)

func (h *handler) upload(w http.ResponseWriter, r *http.Request, req *cacheRequest) {
	if !req.cred.write {
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

	key := req.cred.writeKey(req.hash)
	exists, err := h.objectExists(r, key)
	if err != nil {
		req.outcome = h.storageFailure(w, r, err)
		return
	}
	if exists {
		writeText(w, http.StatusConflict, "Cannot override an existing record")
		req.outcome = outcomeConflict
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(token))
	req.writer = hex.EncodeToString(digest[:])[:12]
	req.uploadedAt = time.Now().UTC().Format(time.RFC3339Nano)
	req.outcome = h.putObject(w, r, req, key)
}

// objectExists is a fast duplicate check. It is not race-free; the conditional
// PUT provides atomicity where storage supports it.
func (h *handler) objectExists(r *http.Request, key string) (bool, error) {
	_, err := h.s3.HeadObject(r.Context(), &s3.HeadObjectInput{Bucket: &h.cfg.Bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if httpStatus(err) == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (h *handler) putObject(w http.ResponseWriter, r *http.Request, req *cacheRequest, key string) outcome {
	hash := req.hash
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rc := http.NewResponseController(w)
	body := startTransfer(receivedBody{r: r.Body, received: h.metrics.received}, h.cfg.StallTimeout, func() {
		cancel()
		expireClientDeadline(rc.SetReadDeadline)
	})
	input := &s3.PutObjectInput{
		Bucket:        &h.cfg.Bucket,
		Key:           &key,
		Body:          body,
		ContentLength: new(r.ContentLength),
		Metadata:      uploadMetadata(req),
	}
	conditional := !h.conditionalUnsupported.Load()
	if conditional {
		input.IfNoneMatch = new("*")
	} else {
		h.metrics.unconditionalUploads.Inc()
	}
	// The request body streams straight to S3 and cannot be replayed, so the
	// payload is unsigned and the SDK must not retry.
	_, err := h.s3.PutObject(ctx, input,
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware),
		func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	stalled := body.finish()

	switch {
	case err == nil:
		writeText(w, http.StatusOK, "Successfully uploaded")
		return outcomeStored
	case stalled:
		return h.uploadStalled(w, r, hash, body)
	case body.failed():
		h.log.WarnContext(r.Context(), "Upload aborted by client", "hash", hash, "error", body.err())
		writeText(w, http.StatusInternalServerError, "Internal server error")
		return outcomeError
	case apiErrorCode(err) == "PreconditionFailed":
		writeText(w, http.StatusConflict, "Cannot override an existing record")
		return outcomeConflict
	case conditional && unsupportedCondition(err):
		// Uploads in flight together may all be rejected; warn only at the switch.
		if h.conditionalUnsupported.CompareAndSwap(false, true) {
			h.log.WarnContext(r.Context(), "Storage does not support conditional writes; "+
				"later uploads are sent without If-None-Match and can overwrite each other",
				"hash", hash, "error", err)
		}
		writeNotStored(w)
		return outcomeDegraded
	default:
		return h.storageFailure(w, r, err)
	}
}

// uploadMetadata identifies the uploader: the token's digest, the upload time
// and, for issued tokens, the repository, namespace and CI run.
func uploadMetadata(req *cacheRequest) map[string]string {
	metadata := map[string]string{"nx-writer": req.writer, "nx-uploaded-at": req.uploadedAt}
	if req.cred.repo.id == "" {
		return metadata
	}
	metadata["nx-repository"] = req.cred.repo.String()
	for name, value := range map[string]string{
		"nx-namespace": req.cred.namespace, "nx-run-id": req.cred.runID, "nx-actor": req.cred.actor,
	} {
		if value != "" {
			metadata[name] = value
		}
	}
	return metadata
}

// receivedBody counts the artifact bytes read from the upload request body.
type receivedBody struct {
	r        io.Reader
	received prometheus.Counter
}

func (b receivedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.received.Add(float64(n))
	return n, err //nolint:wrapcheck // io.Reader contract requires the raw error, including io.EOF
}

// uploadStalled answers an upload that the stall timeout stopped. A failed read
// of the request body means the client stopped sending; otherwise storage
// stopped taking the body or did not answer.
func (h *handler) uploadStalled(w http.ResponseWriter, r *http.Request, hash string, body *transfer) outcome {
	if body.failed() {
		h.reportStall(r, hash, sideClient)
		writeText(w, http.StatusInternalServerError, "Internal server error")
		return outcomeError
	}
	h.reportStall(r, hash, sideStorage)
	return h.storageFailure(w, r, errStalled)
}
