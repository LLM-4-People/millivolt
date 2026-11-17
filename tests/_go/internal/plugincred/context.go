package plugincred

import (
	"context"
	"testing"
)

func TestWithAndFromRoundTrip(t *testing.T) {
	ctx := With(context.Background(), "Authorization", "Bearer sk-test")
	got := From(ctx)
	if got == nil || got.Header != "Authorization" || got.Value != "Bearer sk-test" {
		t.Fatalf("request credential round trip = %+v", got)
	}
	if From(context.Background()) != nil {
		t.Fatal("bare context must not carry a credential")
	}
}
