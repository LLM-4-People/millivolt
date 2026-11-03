package proxy

// Regression test for non_stream_capture_max: a read chunk larger than a small
// configured cap must still contribute a true prefix. The old shape appended
// only whole chunks, so a 16 KiB cap dropped every 32 KiB read and captured
// nothing at all.

import (
	"bytes"
	"testing"
)

func TestAppendCapturePrefix(t *testing.T) {
	const capBytes = 16 << 10
	chunk := bytes.Repeat([]byte("a"), 32<<10)
	buf := appendCapturePrefix(nil, chunk, capBytes)
	if len(buf) != capBytes {
		t.Fatalf("first chunk captured %d bytes, want %d", len(buf), capBytes)
	}
	if !bytes.Equal(buf, chunk[:capBytes]) {
		t.Fatal("captured bytes are not the chunk prefix")
	}
	buf = appendCapturePrefix(buf, []byte("b"), capBytes)
	if len(buf) != capBytes {
		t.Fatalf("capture grew past the cap: %d bytes", len(buf))
	}
	small := appendCapturePrefix(nil, []byte("abc"), capBytes)
	if string(small) != "abc" {
		t.Fatalf("small chunk = %q, want abc", small)
	}
	if got := appendCapturePrefix(nil, []byte("abc"), 0); len(got) != 0 {
		t.Fatalf("zero cap captured %q", got)
	}
}
