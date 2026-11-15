// Package logging is the one owner of process log lines.
//
// The standard log package stays the sink: its flags, writer and the test
// capture remain the runtime's. This wrapper owns only the line shape, so a
// request-scoped line carries the correlation id minted at the read boundary
// and joins its stored row. log.Fatal exits and the earliest flag errors that
// precede the process logger stay direct - they precede what it describes.
package logging

import "log"

// Printf writes one log line. A non-empty id prefixes "request <id>: ", the
// spelling the request path has always used, so every request line joins the
// same row, capture and neighbouring lines. An empty id is a process line.
func Printf(id, format string, args ...any) {
	if id == "" {
		log.Printf(format, args...)
		return
	}
	log.Printf("request %s: "+format, append([]any{id}, args...)...)
}
