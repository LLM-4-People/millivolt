package format

// Round 2 TC1-U2 regression: Anthropic Base64ImageSource accepts only four
// media types, so an unsupported data URL is dropped with a warning.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicImageMediaTypeAllowlist(t *testing.T) {
	t.Run("chat data url", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"data:image/svg+xml;base64,AAAA"}}]}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if !warningField(warnings, "image.media_type") {
			t.Fatalf("unsupported image type recorded no warning: %+v", warnings)
		}
		if strings.Contains(string(out), "base64") || strings.Contains(string(out), "image/") {
			t.Fatalf("anthropic body carried an unsupported image: %s", out)
		}
	})

	t.Run("supported base64", func(t *testing.T) {
		maxTokens := 16
		req := &ir.Request{
			Model: "m", MaxOutputTokens: &maxTokens,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/webp", Data: "AAAA"}},
			}}},
		}
		out, err := EncodeAnthropicRequest(req, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"media_type":"image/webp"`) {
			t.Fatalf("supported image missing from body: %s", out)
		}
	})
}
