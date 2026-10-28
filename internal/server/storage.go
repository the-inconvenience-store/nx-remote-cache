package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

var (
	errBucketNotFound   = errors.New("S3 bucket not found")
	errHeadersTimeout   = errors.New("S3 sent no response headers in time")
	conditionalHeader   = regexp.MustCompile(`(?i)if[- ]?none[- ]?match|conditional (writes?|puts?)`)
	unsupportedFeature  = regexp.MustCompile(`(?i)not (supported|implemented)|unsupported`)
	unsupportedRequests = map[string]bool{"InvalidRequest": true, "InvalidArgument": true, "UnsupportedOperation": true}
)

func newS3Client(ctx context.Context, cfg Config) (*s3.Client, error) {
	httpClient := awshttp.NewBuildableClient().
		WithDialerOptions(func(d *net.Dialer) { d.Timeout = dialTimeout }).
		WithTransportOptions(func(t *http.Transport) {
			t.TLSHandshakeTimeout = tlsHandshakeTimeout
			t.ResponseHeaderTimeout = responseHeaderTimeout
		})
	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
		config.WithHTTPClient(httpClient),
	}
	if cfg.AccessKeyID != "" {
		loadOptions = append(loadOptions, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		if cfg.Endpoint != "" {
			o.BaseEndpoint = new(cfg.Endpoint)
		}
		// Optional checksums would wrap uploads in aws-chunked encoding or force
		// buffering of the one-shot request body; S3-compatible stores vary.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}), nil
}

func httpStatus(err error) int {
	var responseErr *awshttp.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.HTTPStatusCode()
	}
	return 0
}

func apiErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// unsupportedCondition reports whether storage explicitly rejected the
// If-None-Match precondition as a capability it does not implement.
func unsupportedCondition(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.ErrorCode() == "NotImplemented" {
		return true
	}
	message := apiErr.ErrorMessage()
	return unsupportedRequests[apiErr.ErrorCode()] &&
		conditionalHeader.MatchString(message) &&
		unsupportedFeature.MatchString(message)
}

// storageFailure answers a request whose storage call failed. Nx treats only
// GET 404 as a miss and only PUT 403/409 as non-fatal, so by default (fail-open)
// outages degrade instead of failing the Nx run. Every degradation is logged.
func (h *handler) storageFailure(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "Storage unavailable",
		"method", r.Method, "hash", r.PathValue("hash"), "error", err)
	switch {
	case !h.cfg.FailOpen:
		writeText(w, http.StatusServiceUnavailable, "Cache storage unavailable")
	case r.Method == http.MethodPut:
		writeNotStored(w)
	default:
		writeText(w, http.StatusNotFound, "The record was not found")
	}
}

func writeNotStored(w http.ResponseWriter) {
	writeText(w, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
}
