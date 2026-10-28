package server_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

var errNotSent = errors.New("request recorded, not sent")

// requestRecorder captures the address of the request the SDK would send
// instead of sending it.
type requestRecorder struct{ address string }

func (r *requestRecorder) Do(req *http.Request) (*http.Response, error) {
	r.address = req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
	return nil, errNotSent
}

func TestS3RequestAddressStyle(t *testing.T) {
	t.Parallel()
	const endpoint = "https://storage.example.test"
	cases := map[string]struct {
		endpoint string
		style    server.AddressStyle
		want     string
	}{
		"AWS uses virtual-hosted style": {
			"", server.AddressStyleAuto, "https://cache.s3.us-east-1.amazonaws.com/abc",
		},
		"custom endpoint uses path style": {
			endpoint, server.AddressStyleAuto, "https://storage.example.test/cache/abc",
		},
		"AWS forced to path style": {
			"", server.AddressStylePath, "https://s3.us-east-1.amazonaws.com/cache/abc",
		},
		"custom endpoint forced to virtual-hosted style": {
			endpoint, server.AddressStyleVirtualHosted, "https://cache.storage.example.test/abc",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, err := server.NewS3Client(context.Background(), &server.Config{
				AccessTokens: []string{accessToken}, Bucket: testBucket, Region: "us-east-1",
				Endpoint: tc.endpoint, AddressStyle: tc.style, AccessKeyID: "test", SecretAccessKey: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := &requestRecorder{}
			_, err = client.GetObject(t.Context(),
				&s3.GetObjectInput{Bucket: aws.String(testBucket), Key: aws.String("abc")},
				func(o *s3.Options) {
					o.HTTPClient = recorder
					o.Retryer = aws.NopRetryer{}
				})
			if !errors.Is(err, errNotSent) {
				t.Fatalf("GetObject: %v", err)
			}
			if recorder.address != tc.want {
				t.Fatalf("request address %q, want %q", recorder.address, tc.want)
			}
		})
	}
}
