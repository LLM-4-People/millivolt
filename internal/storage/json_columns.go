package storage

// jsonNoValueColumns are the requests TEXT columns that hold a JSON document
// and use SQLite's JSON literal null as the stored no-value, exactly as the
// writer encodes an absent value. Older binaries added some of these columns
// with an empty-string default and some legacy rows still carry ”, which is
// not valid JSON and makes a raw json_each/json_extract consumer fail even
// though the Go decoders treat both spellings as empty. Every admission path
// that copies or admits rows canonicalizes this list before anything else
// reads the staged data; a valid document, including the response_headers
// empty object "{}", passes through byte-for-byte.
var jsonNoValueColumns = []string{"attempts", "response_headers", "tool_names"}

// canonicalJSONExpr wraps expr so NULL and the legacy empty spelling become
// the canonical no-value document. Diagnostics print only the failed column,
// so callers supply their own identifier or qualified column expression.
func canonicalJSONExpr(expr string) string {
	return `CASE WHEN ` + expr + ` IS NULL OR ` + expr + ` = '' THEN 'null' ELSE ` + expr + ` END`
}
