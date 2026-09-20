package proxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"time"
)

// This file is THE artifact owner for any downloaded payload:
// NewArtifactGzipWriter is the compression every saved-gzip artifact crosses
// (the logs export route in cmd/proxy/log.go and the debug capture download
// in internal/proxy/debug.go; any future download surface belongs behind it
// too), and WriteArtifactHeaders composes the download response headers every
// artifact surface stages. The .mvb backup (cmd/proxy/backup.go) is the
// deliberate compression exception: internal/backup already wraps it in
// zstd, and a second compression layer would only spend CPU.
//
// artifactGzipLevel is the codec choice for saved gzip artifacts, not a user
// setting. Level 2 reuses the tradeoff the sibling transit codec
// (internal/web/gzip.go) chose for CPU economy on large bodies: roughly half
// the compression CPU of the default level for modestly more bytes, which
// matters most for the log export that can embed many debug sidecars.
// internal/web/gzip.go stays the transit owner; this file owns artifact bytes.
const artifactGzipLevel = 2

// artifactStampLayout is the one attachment-name stamp layout, shared by
// every artifact surface; the moment is always rendered in UTC so the
// backup, logs and capture filenames agree across surfaces and hosts.
const artifactStampLayout = "20060102-150405"

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

// WriteArtifactHeaders is THE owner of a downloaded artifact's response
// headers, shared by the backup, logs export and capture download surfaces:
// it composes the millivolt-<kind>-<stamp>.<ext> attachment name - the stamp
// is the artifact's moment in artifactStampLayout, always rendered UTC, one
// naming policy for every surface - and stages Content-Disposition, the
// surface's content type and Cache-Control: no-store, so no shared cache
// retains a downloaded artifact. fallback replaces the stamp segment when the
// artifact's moment is unknown (the capture route's damaged-payload row names
// the record id); a surface whose moment is always known passes the empty
// string beside a non-zero moment. Precondition: at must be non-zero or
// fallback non-empty - a zero moment with an empty fallback would publish a
// name with an empty stamp segment. Unreachable today: every surface passes a
// real moment (the backup and export pass time.Now(), the capture passes the
// document's decoded moment or the record-id fallback).
func WriteArtifactHeaders(w http.ResponseWriter, kind, ext, contentType string, at time.Time, fallback string) {
	segment := fallback
	if !at.IsZero() {
		segment = at.UTC().Format(artifactStampLayout)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="millivolt-`+kind+`-`+segment+`.`+ext+`"`)
	w.Header().Set("Cache-Control", "no-store")
}
