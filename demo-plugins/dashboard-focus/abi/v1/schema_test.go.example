package v1

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestSchemaCoversMessageFields catches the common drift where a Go field is
// renamed without updating the published schema projection.
func TestSchemaCoversMessageFields(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(raw)
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Event{}),
		reflect.TypeOf(HTTPFetchRequest{}),
		reflect.TypeOf(HTTPFetchResponse{}),
		reflect.TypeOf(Record{}),
		reflect.TypeOf(ConfigView{}),
		reflect.TypeOf(OperatorCommand{}),
		reflect.TypeOf(Response{}),
		reflect.TypeOf(LogEntry{}),
		reflect.TypeOf(Metric{}),
		reflect.TypeOf(ToolDescriptor{}),
		reflect.TypeOf(UIPanel{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if !strings.Contains(schema, `"`+tag+`"`) {
				t.Errorf("%s.%s: schema.json omits %q", typ.Name(), typ.Field(i).Name, tag)
			}
		}
	}
}

func TestContractNamesTheFixedExportsAndImports(t *testing.T) {
	raw, err := os.ReadFile("contract.md")
	if err != nil {
		t.Fatal(err)
	}
	contract := string(raw)
	for _, name := range []string{"mv_init", "mv_handle", "mv_shutdown", "mv_read_input", "mv_write_output", "mv_emit_metric", "mv_declare_ui_panel", "mv_http_fetch_request", "mv_http_fetch_response"} {
		if !strings.Contains(contract, name) {
			t.Errorf("contract.md omits %q", name)
		}
	}
}
