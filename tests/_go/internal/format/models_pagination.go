package format

import "testing"

func TestModelsPaginationRequiresContinuation(t *testing.T) {
	for _, cursor := range []string{`""`, `null`} {
		if _, err := TranslateAnthropicModels([]byte(`{"data":[{"id":"model-neutral"}],"has_more":true,"last_id":` + cursor + `}`)); err == nil {
			t.Fatalf("accepted has_more without cursor %s", cursor)
		}
	}
	if _, err := TranslateAnthropicModels([]byte(`{"data":[],"has_more":true}`)); err == nil {
		t.Fatal("accepted missing continuation")
	}
	if page, err := TranslateAnthropicModels([]byte(`{"data":[],"has_more":false,"last_id":null}`)); err != nil || page.NextAfter != "" {
		t.Fatalf("empty terminal page: %+v %v", page, err)
	}
}
