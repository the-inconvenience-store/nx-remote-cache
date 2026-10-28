package server

// NewS3Client exposes the storage client so tests can inspect request addresses.
var NewS3Client = newS3Client //nolint:gochecknoglobals // test-only export
