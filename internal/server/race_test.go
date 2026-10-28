//go:build integration || s3contract

package server_test

import (
	"io"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

const raceReleaseTimeout = 30 * time.Second

// assertAtomicUpload races two different artifacts to key past the HEAD
// duplicate check, so only If-None-Match can stop the second writer. Exactly
// one upload must win, and the winner's bytes must survive a later overwrite.
func assertAtomicUpload(t *testing.T, h http.Handler, key string) {
	t.Helper()
	payloads := []string{"first artifact", "second artifact"}
	statuses := racePastDuplicateCheck(t, h, key, payloads)

	sorted := append([]int(nil), statuses...)
	sort.Ints(sorted)
	if sorted[0] != http.StatusOK || sorted[1] != http.StatusConflict {
		t.Fatalf("%s: statuses %v, want one 200 and one 409", key, statuses)
	}
	winner := payloads[0]
	if statuses[1] == http.StatusOK {
		winner = payloads[1]
	}
	status, body := get(t, h, key, accessToken)
	expect(t, status, body, http.StatusOK, winner)

	status, body = put(t, h, key, accessToken, "replacement")
	expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
	status, body = get(t, h, key, accessToken)
	expect(t, status, body, http.StatusOK, winner)
}

// racePastDuplicateCheck uploads every payload to key concurrently and holds
// each body until all requests have passed HEAD and started their PUT.
func racePastDuplicateCheck(t *testing.T, h http.Handler, key string, payloads []string) []int {
	t.Helper()
	var arrived sync.WaitGroup
	arrived.Add(len(payloads))
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()

	statuses := make([]int, len(payloads))
	var uploads sync.WaitGroup
	for i, payload := range payloads {
		uploads.Go(func() {
			first := true
			body := readerFunc(func(p []byte) (int, error) {
				if !first {
					return 0, io.EOF
				}
				first = false
				arrived.Done()
				select {
				case <-release:
				case <-time.After(raceReleaseTimeout):
					return 0, errClientGone
				}
				return copy(p, payload), nil
			})
			statuses[i], _ = do(t, h, call{
				method: http.MethodPut, path: "/v1/cache/" + key, token: accessToken,
				body: body, length: int64(len(payload)),
			})
		})
	}
	uploads.Wait()
	return statuses
}
