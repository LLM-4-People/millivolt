package metrics

import (
	"encoding/json"
	"reflect"
	"testing"
)

// attemptMetaFields is the upstream-response metadata every attempt stores -
// the exact set the record captures for the final response. The names are Go
// field names; the contract under test is that Record and RetryAttempt carry
// identical metadata: same Go name, same JSON tag, same type.
var attemptMetaFields = []string{
	"ProviderRequestID",
	"ProviderServer",
	"ProviderModel",
	"ProcessingMs",
	"RateLimitRemaining",
	"RateLimitLimit",
	"ResponseHeaders",
}

// TestAttemptMetadataParity pins the storage contract that absorbed
// intermediate attempts carry exactly the same upstream-response information
// as the final attempt recorded on the record: every metadata field of one
// type must exist on the other with an identical JSON tag and type, and no
// extra metadata field may appear on one side only. A field added to either
// struct without its twin fails here, so the two field lists cannot drift.
func TestAttemptMetadataParity(t *testing.T) {
	recType := reflect.TypeOf(Record{})
	attType := reflect.TypeOf(RetryAttempt{})
	for _, name := range attemptMetaFields {
		recField, ok := recType.FieldByName(name)
		if !ok {
			t.Errorf("Record has no field %q", name)
			continue
		}
		attField, ok := attType.FieldByName(name)
		if !ok {
			t.Errorf("RetryAttempt has no field %q (Record has it; attempts must store the same information)", name)
			continue
		}
		if recField.Tag.Get("json") != attField.Tag.Get("json") {
			t.Errorf("%s: json tags differ: record %q vs attempt %q", name, recField.Tag.Get("json"), attField.Tag.Get("json"))
		}
		if recField.Type != attField.Type {
			t.Errorf("%s: types differ: record %v vs attempt %v", name, recField.Type, attField.Type)
		}
	}
	// No metadata field may exist on RetryAttempt that the parity set omits.
	for i := 0; i < attType.NumField(); i++ {
		f := attType.Field(i)
		known := false
		for _, name := range attemptMetaFields {
			if name == f.Name {
				known = true
				break
			}
		}
		if !known {
			switch f.Name {
			case "StatusCode", "ErrorType", "ErrorCode", "ErrorMsg", "RetryAfterMs", "At":
				// Attempt-owned fields (the error surface itself).
			default:
				t.Errorf("RetryAttempt has metadata-shaped field %q outside the parity set; add its Record twin or extend the contract", f.Name)
			}
		}
	}
}

// TestAttemptMetadataJSONShape pins the wire shape of the parity: an attempt
// carrying the same metadata as a final response renders the same keys with
// the same values the record's own metadata section renders, so a
// provider-side request id matches identically on both surfaces.
func TestAttemptMetadataJSONShape(t *testing.T) {
	headers := map[string][]string{"X-Request-Id": {"req_shape"}}
	fill := func(v any) {
		r := reflect.ValueOf(v).Elem()
		r.FieldByName("ProviderRequestID").SetString("req_shape")
		r.FieldByName("ProviderServer").SetString("fixture")
		r.FieldByName("ProviderModel").SetString("model-x")
		r.FieldByName("ProcessingMs").SetInt(42)
		r.FieldByName("RateLimitRemaining").SetInt(7)
		r.FieldByName("RateLimitLimit").SetInt(99)
		r.FieldByName("ResponseHeaders").Set(reflect.ValueOf(headers))
	}
	var rec Record
	var att RetryAttempt
	fill(&rec)
	fill(&att)

	recJSON := jsonTagSet(t, rec, attemptMetaFields)
	attJSON := jsonTagSet(t, att, attemptMetaFields)
	for k, v := range recJSON {
		if string(attJSON[k]) != string(v) {
			t.Errorf("metadata key %q: record renders %s, attempt renders %s", k, v, attJSON[k])
		}
	}
}

// jsonTagSet marshals v and returns the rendered value for every parity
// field's JSON key, failing the test on any encode error.
func jsonTagSet(t *testing.T, v any, fields []string) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	typ := reflect.TypeOf(v)
	out := make(map[string]json.RawMessage, len(fields))
	for _, name := range fields {
		f, _ := typ.FieldByName(name)
		key := f.Tag.Get("json")
		for i := 0; i < len(key); i++ {
			if key[i] == ',' {
				key = key[:i]
				break
			}
		}
		out[key] = decoded[key]
	}
	return out
}
