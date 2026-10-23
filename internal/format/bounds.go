package format

import (
	"encoding/json"
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Wire decoders materialize JSON arrays into typed slices, so one compact
// array of empty objects can expand into a large allocation. Every codec
// preflights the arrays it decodes with the shared lexical counter, which
// stops at the first member past the limit and never materializes a part.

// WireCollectionMax is the shared ceiling for a decoded top-level collection
// (messages, tools, input items). It matches the proxy's request preflight so
// a translation never rejects a document the passthrough path accepts; a
// single content-part array keeps the smaller ContentPartMax.
const WireCollectionMax = 10_000

// checkWireArray rejects an over-bound JSON array before its typed decode.
// Absent, null and non-array values pass: the typed decode owns their shape
// errors. A malformed array is reported as over-bound here, matching every
// other caller of the shared counter; a document that does not even parse
// fails the probe unmarshal first and reports its syntax error.
func checkWireArray(raw json.RawMessage, field string, max int) error {
	if len(raw) == 0 || metrics.ArrayWithinLimit(raw, max) {
		return nil
	}
	return fmt.Errorf("%s exceeds %d items", field, max)
}
