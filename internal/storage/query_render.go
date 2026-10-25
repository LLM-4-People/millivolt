package storage

import (
	"encoding/json"
	"sort"
)

// queryProjection is one statement's row-object shape: every distinct column
// name in the order encoding/json writes a map's keys (byte-ordered), each
// carrying the index of the value it renders. The bounded scan builds it once
// per query, which is what lets /metrics/query encode rows straight into the
// response buffer instead of materializing a map per row and marshaling that
// map: keys and values still go through encoding/json, so the bytes cannot
// drift from json.Marshal of the same row map.
type queryProjection struct {
	keys     [][]byte // `"name":` per distinct column, quoted, escaped, colon-terminated
	valueIdx []int
}

// newQueryProjection derives the row shape from a result set's column names.
// A repeated column name collapses exactly like a map assignment: the LAST
// occurrence's value is the one retained, and the key appears once, which is
// what building map[string]any column by column produced.
func newQueryProjection(cols []string) (*queryProjection, error) {
	last := make(map[string]int, len(cols))
	for i, name := range cols {
		last[name] = i
	}
	names := make([]string, 0, len(last))
	for name := range last {
		names = append(names, name)
	}
	sort.Strings(names)
	p := &queryProjection{keys: make([][]byte, len(names)), valueIdx: make([]int, len(names))}
	for i, name := range names {
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		p.keys[i] = append(key, ':')
		p.valueIdx[i] = last[name]
	}
	return p, nil
}

// encodeRow appends one JSON object to dst. enc must write to dst, so a leaf
// encode lands in the row buffer and allocates no buffer of its own.
func (p *queryProjection) encodeRow(dst *jsonSink, enc *json.Encoder, values []any) error {
	dst.buf = append(dst.buf, '{')
	for i, key := range p.keys {
		if i > 0 {
			dst.buf = append(dst.buf, ',')
		}
		dst.buf = append(dst.buf, key...)
		if err := enc.Encode(values[p.valueIdx[i]]); err != nil {
			return err
		}
		// Encode terminates its one value with exactly one newline; the row
		// object must not carry it.
		dst.buf = dst.buf[:len(dst.buf)-1]
	}
	dst.buf = append(dst.buf, '}')
	return nil
}

// jsonSink appends an encoder's writes to a reusable row buffer. It is the one
// difference between a streamed row and json.Marshal of the same map: the
// destination, not the encoding.
type jsonSink struct{ buf []byte }

func (s *jsonSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}
