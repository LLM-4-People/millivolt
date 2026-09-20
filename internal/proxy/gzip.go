package proxy

import (
	"compress/gzip"
	"io"
)

// This file is THE artifact compression owner for any downloaded payload:
// the logs export route (cmd/proxy/log.go) and the debug capture download
// (internal/proxy/debug.go) both compress through NewArtifactGzipWriter, and
// any future download surface belongs behind it too. The .mvb backup
// (cmd/proxy/backup.go) is the deliberate exception: internal/backup already
// wraps it in zstd, and a second compression layer would only spend CPU.
//
// artifactGzipLevel is the codec choice for saved gzip artifacts, not a user
// setting. Level 2 reuses the tradeoff the sibling transit codec
// (internal/web/gzip.go) chose for CPU economy on large bodies: roughly half
// the compression CPU of the default level for modestly more bytes, which
// matters most for the log export that can embed many debug sidecars.
// internal/web/gzip.go stays the transit owner; this file owns artifact bytes.
const artifactGzipLevel = 2

// NewArtifactGzipWriter wraps any writer in the one gzip codec for saved
// artifacts. The caller owns Close on the normal path so the deflate trailer
// flushes; an abandoned stream is a correct truncated artifact.
func NewArtifactGzipWriter(w io.Writer) *gzip.Writer {
	zw, err := gzip.NewWriterLevel(w, artifactGzipLevel)
	if err != nil {
		panic(err) // invalid internal codec choice must fail closed
	}
	return zw
}
