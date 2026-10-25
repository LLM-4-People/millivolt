package config

import (
	"testing"
)

func TestOverlayNonDefaultSkipsBuiltInValues(t *testing.T) {
	src := Default()
	src.MaxRetries = 9
	src.CaptureBodyPreview = true
	dst := Default()
	dst.MaxRetries = 2
	if err := OverlayNonDefault(dst, src); err != nil {
		t.Fatal(err)
	}
	if dst.MaxRetries != 9 || !dst.CaptureBodyPreview {
		t.Fatalf("merged retries=%d preview=%v", dst.MaxRetries, dst.CaptureBodyPreview)
	}
	src.MaxRetries = Default().MaxRetries
	dst = Default()
	dst.MaxRetries = 2
	if err := OverlayNonDefault(dst, src); err != nil {
		t.Fatal(err)
	}
	if dst.MaxRetries != 2 {
		t.Fatalf("default backup retries overwrote live: %d", dst.MaxRetries)
	}
}

func TestDiffKeysNamesModifiedSettings(t *testing.T) {
	a := Default()
	b := Default()
	if keys := DiffKeys(a, b); len(keys) != 0 {
		t.Fatalf("identical configs: %v", keys)
	}
	b.MaxRetries = 9
	keys := DiffKeys(b, a)
	if len(keys) != 1 || keys[0] != "max_retries" {
		t.Fatalf("diff = %v", keys)
	}
}
